package routing_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/routing"
)

var ctx = context.Background()

func name(s string) domain.Name { n, _ := domain.ParseName(s); return n }

var mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}

func branch(n, node string, port int, status domain.BranchStatus) domain.Branch {
	return domain.Branch{Name: name(n), Node: node, Port: port, Status: status, User: n, Password: "pw-" + n, Runtime: mysql84}
}

func TestPlan_ブランチごとにホストグループとユーザーとコメントの規則を作る(t *testing.T) {
	hosts := map[string]string{"n1": "192.0.2.10", "n2": "192.0.2.11"}
	branches := []domain.Branch{
		branch("feature-x", "n1", 40001, domain.BranchReady),
		branch("pr-1", "n2", 40002, domain.BranchResetting),
		branch("broken", "n1", 40003, domain.BranchFailed),
		branch("away", "n3", 40004, domain.BranchReady), // node unknown
	}
	old := branch("legacy", "n1", 40005, domain.BranchReady)
	old.User = "branch" // not yet given its own user
	branches = append(branches, old)
	users := []domain.RouteUser{{Name: "app", Password: "apw", DefaultBranch: name("feature-x")}, {Name: "ghost", Password: "g", DefaultBranch: name("deleted")}}

	c := routing.Plan(branches, hosts, users).MySQL
	hg := domain.Hostgroups([]domain.Name{name("feature-x"), name("pr-1"), name("away"), name("legacy")})

	if len(c.Servers) != 3 {
		t.Fatalf("servers = %+v", c.Servers)
	}
	for _, s := range c.Servers {
		if s.Hostgroup == hg[name("pr-1")] && (s.Host != "192.0.2.11" || s.Port != 40002) {
			t.Errorf("pr-1 server = %+v", s)
		}
	}
	byUser := map[string]routing.User{}
	for _, u := range c.Users {
		byUser[u.Name] = u
	}
	if len(byUser) != 4 || byUser["feature-x"].Hostgroup != hg[name("feature-x")] || byUser["feature-x"].Password != "pw-feature-x" {
		t.Errorf("users = %+v", c.Users)
	}
	if byUser["app"].Hostgroup != hg[name("feature-x")] || byUser["ghost"].Hostgroup != routing.NoHostgroup {
		t.Errorf("route users = %+v", c.Users)
	}
	if _, ok := byUser["legacy"]; ok {
		t.Error("a branch without its own user cannot be reached by user name yet")
	}
	first, last := c.Rules[0], c.Rules[len(c.Rules)-1]
	if first.ID != 1 || first.FlagOut == 0 || first.Apply || last.ID != routing.CatchAllRule || last.Error != routing.UnknownBranch || last.FlagIn != first.FlagOut {
		t.Errorf("rules = %+v", c.Rules)
	}
	found := false
	for _, r := range c.Rules {
		if r.Pattern == "db_branch='feature-x'" {
			found = r.Hostgroup == hg[name("feature-x")] && r.Apply && r.FlagIn == first.FlagOut
		}
	}
	if !found || len(c.Rules) != 5 {
		t.Errorf("rules = %+v", c.Rules)
	}
}

type fakeAdmin struct {
	applied []routing.Config
	err     error
}

func (f *fakeAdmin) Apply(_ context.Context, c routing.Config) error {
	f.applied = append(f.applied, c)
	return f.err
}

type fakeNodes struct {
	nodes []domain.Node
	err   error
}

func (f *fakeNodes) PutNode(context.Context, domain.Node) error       { return nil }
func (f *fakeNodes) DeleteNode(context.Context, string) error         { return nil }
func (f *fakeNodes) ListNodes(context.Context) ([]domain.Node, error) { return f.nodes, f.err }

func newService(t *testing.T) (*routing.Service, *filestore.Store, *fakeAdmin) {
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	a := &fakeAdmin{}
	return &routing.Service{Store: st, Users: st, Node: "n1", Host: "192.0.2.10", Admin: a}, st, a
}

func TestSync_変わったときだけ反映する(t *testing.T) {
	s, st, a := newService(t)
	_ = st.PutBranch(ctx, branch("x", "", 40001, domain.BranchReady))
	if changed, err := s.Sync(ctx); err != nil || !changed {
		t.Fatalf("first: %v %v", changed, err)
	}
	if a.applied[0].MySQL.Servers[0].Host != "192.0.2.10" {
		t.Errorf("a single node's branch reaches its own host: %+v", a.applied[0].MySQL.Servers)
	}
	if changed, _ := s.Sync(ctx); changed || len(a.applied) != 1 {
		t.Error("unchanged config applied again")
	}
	_ = st.PutBranch(ctx, branch("y", "n1", 40002, domain.BranchReady))
	if changed, _ := s.Sync(ctx); !changed {
		t.Error("new branch not applied")
	}
	a.err = errors.New("proxy down")
	_ = st.PutBranch(ctx, branch("z", "n1", 40003, domain.BranchReady))
	if _, err := s.Sync(ctx); err == nil {
		t.Error("want error")
	}
	a.err = nil
	if changed, _ := s.Sync(ctx); !changed {
		t.Error("a failed apply must be retried")
	}
}

func TestSync_クラスタでは各ノードのホストへ向ける(t *testing.T) {
	s, st, a := newService(t)
	s.Nodes = &fakeNodes{nodes: []domain.Node{{Name: "n2", Host: "192.0.2.11"}}}
	_ = st.PutBranch(ctx, branch("x", "n2", 40001, domain.BranchReady))
	if _, err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if a.applied[0].MySQL.Servers[0].Host != "192.0.2.11" {
		t.Errorf("servers = %+v", a.applied[0].MySQL.Servers)
	}
	s.Nodes = &fakeNodes{err: errors.New("raft down")}
	if _, err := s.Sync(ctx); err == nil {
		t.Error("want error")
	}
}

func TestRouteUsers_追加と一覧と削除(t *testing.T) {
	s, st, _ := newService(t)
	_ = st.PutBranch(ctx, branch("feature-x", "n1", 40001, domain.BranchReady))
	if err := s.AddUser(ctx, domain.RouteUser{Name: "app", Password: "p", DefaultBranch: name("feature-x")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(ctx, domain.RouteUser{Name: "app2", Password: "p", DefaultBranch: name("nope")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown default: %v", err)
	}
	if err := s.AddUser(ctx, domain.RouteUser{Name: "feature-x", Password: "p", DefaultBranch: name("feature-x")}); !errors.Is(err, domain.ErrInvalidRouteUser) {
		t.Errorf("clash: %v", err)
	}
	if list, _ := s.ListUsers(ctx); len(list) != 1 || list[0].Name != "app" {
		t.Errorf("list = %+v", list)
	}
	if err := s.RemoveUser(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListUsers(ctx); len(list) != 0 {
		t.Errorf("after remove = %+v", list)
	}
}

func TestPlan_PostgreSQLのブランチはPostgreSQL側に置く(t *testing.T) {
	pg := branch("pg-x", "n1", 40010, domain.BranchReady)
	pg.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}
	branches := []domain.Branch{branch("my-x", "n1", 40001, domain.BranchReady), pg}
	users := []domain.RouteUser{{Name: "pgapp", Password: "p", DefaultBranch: name("pg-x")}}
	c := routing.Plan(branches, map[string]string{"n1": "192.0.2.10"}, users)
	if len(c.MySQL.Servers) != 1 || c.MySQL.Servers[0].Port != 40001 || len(c.Postgres.Servers) != 1 || c.Postgres.Servers[0].Port != 40010 {
		t.Errorf("servers: mysql %+v pg %+v", c.MySQL.Servers, c.Postgres.Servers)
	}
	if len(c.Postgres.Users) != 2 || len(c.MySQL.Users) != 1 {
		t.Errorf("users: mysql %+v pg %+v", c.MySQL.Users, c.Postgres.Users)
	}
	if len(c.Postgres.Rules) != 3 || len(c.MySQL.Rules) != 3 {
		t.Errorf("rules: mysql %+v pg %+v", c.MySQL.Rules, c.Postgres.Rules)
	}
}

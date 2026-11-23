package routing_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
		branch("merging", "n1", 40006, domain.BranchMerging),
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
	last := c.Rules[len(c.Rules)-1]
	entries := 0
	var flag int
	for _, r := range c.Rules {
		if r.FlagOut != 0 {
			entries++
			flag = r.FlagOut
			if r.Apply || r.Username == "" {
				t.Errorf("entry rule = %+v", r)
			}
		}
	}
	if entries != 2 || last.ID != routing.CatchAllRule || last.Error != routing.UnknownBranch || last.FlagIn != flag {
		t.Errorf("rules = %+v", c.Rules)
	}
	found := false
	for _, r := range c.Rules {
		if r.Hostgroup == hg[name("feature-x")] && r.FlagIn == flag {
			found = r.Apply && r.Pattern == `/\*\s*db_branch\s*=\s*'feature-x'\s*\*/`
		}
	}
	if !found || len(c.Rules) != 6 {
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
	if len(c.Postgres.Rules) != 3 || len(c.MySQL.Rules) != 2 {
		t.Errorf("rules: mysql %+v pg %+v", c.MySQL.Rules, c.Postgres.Rules)
	}
}

func TestPlan_復元中のブランチもルートを保つ(t *testing.T) {
	hosts := map[string]string{"n1": "192.0.2.10"}
	c := routing.Plan([]domain.Branch{branch("pr-2", "n1", 40001, domain.BranchRestoring), branch("gone", "n1", 40002, domain.BranchDeleting)}, hosts, nil).MySQL
	if len(c.Servers) != 1 || c.Servers[0].Port != 40001 {
		t.Errorf("servers = %+v: a restore is brief, the route stays like it does for a reset", c.Servers)
	}
}

func TestPlan_外部ランタイムのブランチはMySQLにもPostgreSQLにも入らない(t *testing.T) {
	ext := branch("ext-x", "n1", 40020, domain.BranchReady)
	ext.Runtime = domain.RuntimeRef{Engine: "mongo", Version: "7"}
	hosts := map[string]string{"n1": "192.0.2.10"}
	users := []domain.RouteUser{{Name: "extapp", Password: "p", DefaultBranch: name("ext-x")}}
	c := routing.Plan([]domain.Branch{ext}, hosts, users)
	if len(c.MySQL.Servers)+len(c.MySQL.Users)+len(c.Postgres.Servers)+len(c.Postgres.Users) != 0 {
		t.Errorf("an external engine is not behind the proxy: %+v", c)
	}
}

func TestPlan_デフォルトブランチ削除済みのPostgreSQLルートユーザーがMySQLへ移動しない(t *testing.T) {
	users := []domain.RouteUser{{Name: "pgapp", Password: "p", DefaultBranch: name("deleted"), Engine: domain.Postgres}}
	c := routing.Plan(nil, nil, users)
	if len(c.MySQL.Users) != 0 || len(c.Postgres.Users) != 1 || c.Postgres.Users[0].Hostgroup != routing.NoHostgroup {
		t.Errorf("mysql %+v pg %+v", c.MySQL.Users, c.Postgres.Users)
	}
}

func TestPlan_コメントルーティングは共有ルートユーザーだけに許しブランチ専用ユーザーの越境を許さない(t *testing.T) {
	branches := []domain.Branch{branch("a-1", "n1", 40001, domain.BranchReady), branch("b-1", "n1", 40002, domain.BranchReady)}
	users := []domain.RouteUser{{Name: "app", Password: "p", DefaultBranch: name("a-1")}}
	c := routing.Plan(branches, map[string]string{"n1": "192.0.2.10"}, users).MySQL
	entry := map[string]bool{}
	for _, r := range c.Rules {
		if r.FlagOut != 0 && r.FlagIn == 0 {
			entry[r.Username] = true
			if r.Pattern != `/\*\s*db_branch\s*=` {
				t.Errorf("the entry rule must anchor on comment syntax: %q", r.Pattern)
			}
		}
		if r.FlagIn != 0 && r.Hostgroup != 0 && !strings.HasPrefix(r.Pattern, `/\*`) || !strings.HasSuffix(r.Pattern, `*/`) && r.Hostgroup != 0 {
			t.Errorf("a branch rule must match a comment, not a bare string: %q", r.Pattern)
		}
	}
	if len(entry) != 1 || !entry["app"] {
		t.Errorf("only the route user may route by comment: %v", entry)
	}
}

func TestPlan_RouteUserと同名のブランチユーザーは重複登録せずRouteUserを優先する(t *testing.T) {
	branches := []domain.Branch{branch("app", "n1", 40001, domain.BranchReady)}
	users := []domain.RouteUser{{Name: "app", Password: "shared", DefaultBranch: name("app")}}
	c := routing.Plan(branches, map[string]string{"n1": "192.0.2.10"}, users).MySQL
	if len(c.Users) != 1 || c.Users[0].Password != "shared" {
		t.Errorf("users = %+v", c.Users)
	}
}

func TestAddUser_既定ブランチのエンジンを覚える(t *testing.T) {
	s, st, _ := newService(t)
	pg := branch("pg-x", "n1", 40010, domain.BranchReady)
	pg.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}
	_ = st.PutBranch(ctx, pg)
	if err := s.AddUser(ctx, domain.RouteUser{Name: "pgapp", Password: "p", DefaultBranch: name("pg-x")}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListUsers(ctx)
	if len(list) != 1 || list[0].Engine != domain.Postgres {
		t.Errorf("users = %+v", list)
	}
}

func TestPlan_コメントの入口ルールはブランチ別ルールより先に評価される(t *testing.T) {
	branches := []domain.Branch{branch("a-1", "n1", 40001, domain.BranchReady)}
	users := []domain.RouteUser{{Name: "app", Password: "p", DefaultBranch: name("a-1")}}
	c := routing.Plan(branches, map[string]string{"n1": "192.0.2.10"}, users).MySQL
	// The proxy walks rules once in ID order, so a rule waiting on the flag
	// must come after the one that sets it.
	setAt := -1
	for _, r := range c.Rules {
		if r.FlagOut != 0 {
			setAt = r.ID
		} else if r.FlagIn != 0 && (setAt < 0 || r.ID < setAt) {
			t.Errorf("rule %d (%q) is checked before the entry rule %d", r.ID, r.Pattern, setAt)
		}
	}
}

func TestAddUser_ルートユーザーの数には上限がある(t *testing.T) {
	s, st, _ := newService(t)
	_ = st.PutBranch(ctx, branch("feature-x", "n1", 40001, domain.BranchReady))
	for i := 0; i < routing.MaxRouteUsers; i++ {
		if err := s.AddUser(ctx, domain.RouteUser{Name: fmt.Sprintf("u%d", i), Password: "p", DefaultBranch: name("feature-x")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddUser(ctx, domain.RouteUser{Name: "one-too-many", Password: "p", DefaultBranch: name("feature-x")}); !errors.Is(err, domain.ErrInvalidRouteUser) {
		t.Errorf("over the limit: %v", err)
	}
	if err := s.AddUser(ctx, domain.RouteUser{Name: "u0", Password: "new", DefaultBranch: name("feature-x")}); err != nil {
		t.Errorf("replacing one is fine: %v", err)
	}
}

func TestPlan_別エンジンのルートユーザーと同名でもブランチのログインを落とさない(t *testing.T) {
	pg := branch("x", "n1", 40010, domain.BranchReady)
	pg.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}
	my := branch("other", "n1", 40001, domain.BranchReady)
	users := []domain.RouteUser{{Name: "x", Password: "p", DefaultBranch: name("other")}}
	c := routing.Plan([]domain.Branch{pg, my}, map[string]string{"n1": "192.0.2.10"}, users)
	if len(c.Postgres.Users) != 1 || c.Postgres.Users[0].Password != "pw-x" {
		t.Errorf("pg users = %+v", c.Postgres.Users)
	}
}

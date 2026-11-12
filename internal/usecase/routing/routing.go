// Package routing keeps each node's proxy (ProxySQL) in line with the
// cluster: one hostgroup per branch, a user per branch, shared route users,
// and query rules for /*db_branch='name'*/ comments (design.md §8).
package routing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// The routing types are the public ones a Router module receives.
type (
	Server = ext.RouteServer
	User   = ext.RouteUser
	Rule   = ext.RouteRule
	Tables = ext.RouteTables
	Config = ext.RouteConfig
)

// Admin applies a Config to this node's proxy.
type Admin interface {
	Apply(ctx context.Context, c Config) error
}

const (
	commentFlag = 100
	// CatchAllRule rejects comments naming no known branch, after every
	// branch rule (IDs are hostgroups, all below it).
	CatchAllRule = 2_000_000_000
	// NoHostgroup has no servers: a route user whose default branch is gone
	// can still use comments, but nothing without one.
	NoHostgroup = 0
)

// UnknownBranch is the error a query gets when its comment names no branch.
const UnknownBranch = "db-branch: the db_branch comment names no branch"

func routable(b domain.Branch) bool {
	return b.Status == domain.BranchReady || b.Status == domain.BranchResetting || b.Status == domain.BranchRestoring
}

// Plan computes the proxy config. hosts maps node names to the address the
// proxy uses to reach that node's servers ("" is this node).
func Plan(branches []domain.Branch, hosts map[string]string, users []domain.RouteUser) Config {
	var names []domain.Name
	engine := map[domain.Name]domain.Engine{}
	for _, b := range branches {
		if routable(b) {
			names = append(names, b.Name)
			engine[b.Name] = b.Runtime.Engine
		}
	}
	hg := domain.Hostgroups(names)
	c := Config{}
	tables := func(e domain.Engine) *Tables {
		if e == domain.Postgres {
			return &c.Postgres
		}
		return &c.MySQL
	}
	for _, b := range branches {
		id, ok := hg[b.Name]
		host, known := hosts[b.Node]
		if !ok || !known {
			continue
		}
		t := tables(b.Runtime.Engine)
		t.Servers = append(t.Servers, Server{Hostgroup: id, Host: host, Port: b.Port})
		if b.User == b.Name.String() {
			t.Users = append(t.Users, User{Name: b.User, Password: b.Password, Hostgroup: id})
		}
		t.Rules = append(t.Rules, Rule{ID: id, FlagIn: commentFlag, Pattern: "db_branch='" + b.Name.String() + "'", Hostgroup: id, Apply: true})
	}
	for _, u := range users {
		id, ok := hg[u.DefaultBranch]
		if !ok {
			id = NoHostgroup
		}
		t := tables(engine[u.DefaultBranch])
		t.Users = append(t.Users, User{Name: u.Name, Password: u.Password, Hostgroup: id})
	}
	for _, t := range []*Tables{&c.MySQL, &c.Postgres} {
		t.Rules = append(t.Rules,
			Rule{ID: 1, Pattern: "db_branch=", FlagOut: commentFlag},
			Rule{ID: CatchAllRule, FlagIn: commentFlag, Pattern: "db_branch=", Error: UnknownBranch, Apply: true})
		sort.Slice(t.Servers, func(i, j int) bool { return t.Servers[i].Hostgroup < t.Servers[j].Hostgroup })
		sort.Slice(t.Users, func(i, j int) bool { return t.Users[i].Name < t.Users[j].Name })
		sort.Slice(t.Rules, func(i, j int) bool { return t.Rules[i].ID < t.Rules[j].ID })
	}
	return c
}

// Service syncs the local proxy and manages route users.
type Service struct {
	Store domain.Store
	Users domain.RouteUserStore
	// Nodes is nil on a single node.
	Nodes domain.NodeStore
	// Node and Host are this node's name and address.
	Node, Host string
	Admin      Admin

	mu   sync.Mutex
	last [32]byte
}

func (s *Service) hosts(ctx context.Context) (map[string]string, error) {
	hosts := map[string]string{"": s.Host, s.Node: s.Host}
	if s.Nodes == nil {
		return hosts, nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	for _, n := range nodes {
		if n.Host != "" {
			hosts[n.Name] = n.Host
		}
	}
	return hosts, err
}

// Sync applies the current config to the proxy if it changed since the last
// successful apply, and reports whether it applied.
func (s *Service) Sync(ctx context.Context) (bool, error) {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return false, err
	}
	users, err := s.Users.ListRouteUsers(ctx)
	if err != nil {
		return false, err
	}
	hosts, err := s.hosts(ctx)
	if err != nil {
		return false, err
	}
	c := Plan(branches, hosts, users)
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sum == s.last {
		return false, nil
	}
	if err := s.Admin.Apply(ctx, c); err != nil {
		return false, err
	}
	s.last = sum
	return true, nil
}

// AddUser adds or replaces a route user.
func (s *Service) AddUser(ctx context.Context, u domain.RouteUser) error {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	if err := u.Validate(branches); err != nil {
		return err
	}
	if _, err := s.Store.GetBranch(ctx, u.DefaultBranch); err != nil {
		return fmt.Errorf("default branch: %w", err)
	}
	return s.Users.PutRouteUser(ctx, u)
}

// RemoveUser deletes a route user.
func (s *Service) RemoveUser(ctx context.Context, name string) error {
	return s.Users.DeleteRouteUser(ctx, name)
}

// ListUsers returns the route users.
func (s *Service) ListUsers(ctx context.Context) ([]domain.RouteUser, error) {
	return s.Users.ListRouteUsers(ctx)
}

// ErrNoProxy is returned when this node has no proxy to configure.
var ErrNoProxy = errors.New("no proxy on this node")

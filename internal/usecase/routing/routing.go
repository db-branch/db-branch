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
	"regexp"
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

// MaxRouteUsers is how many route users there can be: each has an entry rule
// whose ID (1..MaxRouteUsers) must be below every branch rule, because the
// proxy walks rules once in ID order and hostgroup IDs start at 1000.
const MaxRouteUsers = 999

// Comment syntax: only a real /* ... */ comment naming the branch matches, so
// a string literal that merely contains the words is left alone.
const (
	commentStart = `/\*\s*db_branch\s*=`
	commentEnd   = `\s*\*/`
)

func branchPattern(n domain.Name) string {
	return commentStart + `\s*'` + regexp.QuoteMeta(n.String()) + `'` + commentEnd
}

// Plan computes the proxy config. hosts maps node names to the address the
// proxy uses to reach that node's servers ("" is this node). Only MySQL and
// PostgreSQL go through the proxy; a branch of an external engine has no
// table. Comment routing is for route users alone: a branch's own login
// stays on its branch, so it cannot reach another branch by a comment.
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
		switch e {
		case domain.MySQL:
			return &c.MySQL
		case domain.Postgres:
			return &c.Postgres
		}
		return nil
	}
	users = append([]domain.RouteUser(nil), users...)
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	// A route user's table: its default branch's engine while that exists,
	// else the one it was made with (MySQL for users stored before that).
	tableOf := func(u domain.RouteUser) *Tables {
		e := u.Engine
		if be, live := engine[u.DefaultBranch]; live {
			e = be
		}
		if e == "" {
			e = domain.MySQL
		}
		return tables(e)
	}
	shared := map[*Tables]map[string]bool{&c.MySQL: {}, &c.Postgres: {}}
	for _, u := range users {
		if t := tableOf(u); t != nil {
			shared[t][u.Name] = true
		}
	}
	for _, b := range branches {
		id, ok := hg[b.Name]
		host, known := hosts[b.Node]
		t := tables(b.Runtime.Engine)
		if !ok || !known || t == nil {
			continue
		}
		t.Servers = append(t.Servers, Server{Hostgroup: id, Host: host, Port: b.Port})
		// A route user of the same name keeps the name: the proxy takes one
		// login per name.
		if b.User == b.Name.String() && !shared[t][b.User] {
			t.Users = append(t.Users, User{Name: b.User, Password: b.Password, Hostgroup: id})
		}
		t.Rules = append(t.Rules, Rule{ID: id, FlagIn: commentFlag, Pattern: branchPattern(b.Name), Hostgroup: id, Apply: true})
	}
	entries := map[*Tables]int{}
	for _, u := range users {
		id, ok := hg[u.DefaultBranch]
		if !ok {
			id = NoHostgroup
		}
		t := tableOf(u)
		if t == nil {
			continue
		}
		t.Users = append(t.Users, User{Name: u.Name, Password: u.Password, Hostgroup: id})
		if entries[t] >= MaxRouteUsers {
			continue
		}
		t.Rules = append(t.Rules, Rule{ID: entries[t] + 1, Username: u.Name, Pattern: commentStart, FlagOut: commentFlag})
		entries[t]++
	}
	for _, t := range []*Tables{&c.MySQL, &c.Postgres} {
		t.Rules = append(t.Rules,
			Rule{ID: CatchAllRule, FlagIn: commentFlag, Pattern: commentStart, Error: UnknownBranch, Apply: true})
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
	def, err := s.Store.GetBranch(ctx, u.DefaultBranch)
	if err != nil {
		return fmt.Errorf("default branch: %w", err)
	}
	u.Engine = def.Runtime.Engine
	existing, err := s.Users.ListRouteUsers(ctx)
	if err != nil {
		return err
	}
	replacing := false
	for _, e := range existing {
		replacing = replacing || e.Name == u.Name
	}
	if !replacing && len(existing) >= MaxRouteUsers {
		return errors.Join(domain.ErrInvalidRouteUser, fmt.Errorf("at most %d route users", MaxRouteUsers))
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

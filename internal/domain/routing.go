package domain

import (
	"context"
	"errors"
	"hash/crc32"
	"sort"
)

// RouteUser is a database user shared by every branch (usually one the base
// already had, such as the application's). Connected through the proxy it
// reaches DefaultBranch, or the branch a query names in a
// /*db_branch='name'*/ comment.
type RouteUser struct {
	Name          string `json:"name"`
	Password      string `json:"password"`
	DefaultBranch Name   `json:"default_branch"`
	// Engine is the proxy table the user lives in. It outlives DefaultBranch:
	// empty (users stored before the field) means MySQL.
	Engine Engine `json:"engine,omitempty"`
}

// ErrInvalidRouteUser is returned for a route user that cannot be configured.
var ErrInvalidRouteUser = errors.New("invalid route user")

// Validate checks the user can be configured without clashing with branch users.
func (u RouteUser) Validate(branches []Branch) error {
	if u.Name == "" || len(u.Name) > 32 || u.Password == "" {
		return errors.Join(ErrInvalidRouteUser, errors.New("a name of up to 32 characters and a password are required"))
	}
	for _, b := range branches {
		if b.Name.String() == u.Name || b.User == u.Name {
			return errors.Join(ErrInvalidRouteUser, errors.New("the name is taken by branch "+b.Name.String()))
		}
	}
	return nil
}

// RouteUserStore keeps route users (single node: the state file; cluster: Raft).
type RouteUserStore interface {
	ListRouteUsers(ctx context.Context) ([]RouteUser, error)
	PutRouteUser(ctx context.Context, u RouteUser) error
	DeleteRouteUser(ctx context.Context, name string) error
}

// Hostgroups gives every branch a proxy hostgroup ID derived from its name,
// so the ID stays the same while other branches come and go and every node
// computes the same one. IDs collide only by chance; the later name (in
// sorted order) then takes the next free ID.
func Hostgroups(names []Name) map[Name]int {
	sorted := append([]Name(nil), names...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	used := map[int]bool{}
	out := map[Name]int{}
	for _, n := range sorted {
		id := 1000 + int(crc32.ChecksumIEEE([]byte(n.String()))%1_000_000_000)
		for used[id] {
			id++
		}
		used[id] = true
		out[n] = id
	}
	return out
}

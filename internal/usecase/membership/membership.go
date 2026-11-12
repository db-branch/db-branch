// Package membership decides which cluster members vote in Raft. Only an odd
// number of them, at most MaxVoters, vote; the rest follow as non-voters and
// are promoted when a voter is lost (see docs/cluster-plan.md).
package membership

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	// MaxVoters caps the voters: more only slows every write down.
	MaxVoters = 5
	// Stable is how long a non-voter must have been healthy before promotion.
	Stable = 30 * time.Second
	// DemoteAfter is how long a voter must have been silent before it is
	// replaced.
	DemoteAfter = 2 * time.Minute
)

// Server is one member of the Raft configuration (its ID is the node name).
type Server struct {
	ID    string
	Voter bool
}

// Members is the Raft configuration. Promote and Demote return once the
// change is committed.
type Members interface {
	IsLeader() bool
	Servers(ctx context.Context) ([]Server, error)
	Promote(ctx context.Context, id string) error
	Demote(ctx context.Context, id string) error
}

// Service is the leader's membership loop body.
type Service struct {
	Members Members
	// Self is this node's name; the leader is never demoted.
	Self  string
	Nodes func(ctx context.Context) ([]domain.Node, error)
	Clock func() time.Time
	// DownAfter is how long without a heartbeat marks a node unhealthy.
	DownAfter time.Duration

	mu           sync.Mutex
	healthySince map[string]time.Time
}

// TargetVoters is the largest odd number not above min(healthy, MaxVoters),
// and at least 1.
func TargetVoters(healthy int) int {
	n := min(healthy, MaxVoters)
	if n < 1 {
		return 1
	}
	if n%2 == 0 {
		n--
	}
	return n
}

func majority(n int) int { return n/2 + 1 }

// Reconcile makes at most one membership change and reports whether it did.
//
//  1. A voter silent for DemoteAfter is demoted when a stable healthy
//     non-voter can take its place, or when there are too many voters.
//  2. With more voters than TargetVoters(members) (the cluster shrank, or
//     there are more than MaxVoters), one is demoted (a silent one first,
//     never the leader).
//  3. With fewer than TargetVoters(healthy members), the stable healthy non-voter that has been healthy longest
//     is promoted.
//
// No change may leave fewer healthy voters than a majority of the new
// configuration.
func (s *Service) Reconcile(ctx context.Context) (bool, error) {
	if !s.Members.IsLeader() {
		return false, nil
	}
	servers, err := s.Members.Servers(ctx)
	if err != nil {
		return false, err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return false, err
	}
	now := s.Clock()
	seen := map[string]time.Time{}
	for _, n := range nodes {
		seen[n.Name] = n.LastSeen
	}
	healthy := func(id string) bool {
		at, ok := seen[id]
		return ok && now.Sub(at) <= s.DownAfter
	}
	silentLong := func(id string) bool {
		at, ok := seen[id]
		return ok && now.Sub(at) > DemoteAfter
	}

	// Remember since when each member has been healthy without a break.
	s.mu.Lock()
	if s.healthySince == nil {
		s.healthySince = map[string]time.Time{}
	}
	nHealthy := 0
	for _, sv := range servers {
		if !healthy(sv.ID) {
			delete(s.healthySince, sv.ID)
			continue
		}
		nHealthy++
		if _, ok := s.healthySince[sv.ID]; !ok {
			s.healthySince[sv.ID] = now
		}
	}
	since := map[string]time.Time{}
	for k, v := range s.healthySince {
		since[k] = v
	}
	s.mu.Unlock()

	var voters, ready []Server
	healthyVoters := 0
	for _, sv := range servers {
		switch {
		case sv.Voter:
			voters = append(voters, sv)
			if healthy(sv.ID) {
				healthyVoters++
			}
		case healthy(sv.ID) && now.Sub(since[sv.ID]) >= Stable:
			ready = append(ready, sv)
		}
	}
	sort.Slice(voters, func(i, j int) bool { return voters[i].ID < voters[j].ID })
	sort.Slice(ready, func(i, j int) bool {
		a, b := since[ready[i].ID], since[ready[j].ID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return ready[i].ID < ready[j].ID
	})
	target := TargetVoters(nHealthy)
	// Voters are cut for being too many only when the cluster itself shrank
	// (members removed, or more than MaxVoters), not because some members are
	// briefly unreachable: an outage must not shrink the voter set.
	most := TargetVoters(len(servers))

	// A demotion of a healthy voter must leave a healthy majority.
	demote := func(v Server) (bool, error) {
		return true, s.Members.Demote(ctx, v.ID)
	}
	safeDemote := func(v Server) bool {
		h := healthyVoters
		if healthy(v.ID) {
			h--
		}
		return h >= majority(len(voters)-1)
	}

	// 1. Replace a voter that has been silent for long.
	for _, v := range voters {
		if v.ID != s.Self && silentLong(v.ID) && (len(ready) > 0 || len(voters) > most) {
			return demote(v)
		}
	}
	// 2. Too many voters: a silent one goes first, then the last by name.
	if len(voters) > most {
		var pick *Server
		for i := len(voters) - 1; i >= 0; i-- {
			v := voters[i]
			if v.ID == s.Self || !safeDemote(v) {
				continue
			}
			if !healthy(v.ID) {
				pick = &voters[i]
				break
			}
			if pick == nil {
				pick = &voters[i]
			}
		}
		if pick != nil {
			return demote(*pick)
		}
		return false, nil
	}
	// 3. Too few: promote the longest-healthy ready non-voter, if the
	// larger configuration still has a healthy majority.
	if len(voters) < target && len(ready) > 0 && healthyVoters+1 >= majority(len(voters)+1) {
		return true, s.Members.Promote(ctx, ready[0].ID)
	}
	return false, nil
}

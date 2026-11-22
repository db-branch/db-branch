// Package notify tells people what needs them: branches about to expire,
// operations that failed, nodes that went down or came back. The leader
// checks the cluster state and sends each notice once.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

// Notice is one thing to tell.
type Notice struct {
	Event   string    `json:"event"` // branch.expiring, operation.failed, node.down, node.up
	Level   string    `json:"level"` // info, warning, error
	Subject string    `json:"subject"`
	Text    string    `json:"text"`
	Time    time.Time `json:"time"`
	// Owner is the branch's owner, when about a branch.
	Owner string `json:"owner,omitempty"`
}

// Sender delivers notices (Slack, a webhook).
type Sender interface {
	Send(ctx context.Context, n Notice) error
}

// State is the cluster state notices come from.
type State interface {
	ListBranches(ctx context.Context) ([]domain.Branch, error)
	ListOperations(ctx context.Context) ([]operation.Operation, error)
	ListNodes(ctx context.Context) ([]domain.Node, error)
}

// Service checks the state and sends new notices.
type Service struct {
	State   State
	Senders []Sender
	Clock   func() time.Time
	// Warn is how long before expiry to warn (0: no expiry warnings).
	Warn func() time.Duration
	// DownAfter is when a node counts as down (0: no node notices).
	DownAfter time.Duration
	// Since: operations that failed before it are not reported.
	Since time.Time

	mu   sync.Mutex
	sent map[string]bool
	down map[string]bool
}

func (s *Service) once(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent == nil {
		s.sent = map[string]bool{}
	}
	if s.sent[key] {
		return false
	}
	s.sent[key] = true
	return true
}

// short writes a duration in minutes: 1h, 1h30m, 45m.
func short(d time.Duration) string {
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Check sends the notices that are new since the last check.
func (s *Service) Check(ctx context.Context) error {
	now := s.Clock()
	var notices []Notice
	branches, err := s.State.ListBranches(ctx)
	if err != nil {
		return err
	}
	if warn := s.Warn(); warn > 0 {
		for _, b := range branches {
			if b.ExpiresAt == nil || b.ExpiresAt.Sub(now) > warn || !b.ExpiresAt.After(now) {
				continue
			}
			if s.once("expiring/" + b.Name.String() + "/" + b.ExpiresAt.Format(time.RFC3339)) {
				notices = append(notices, Notice{Event: "branch.expiring", Level: "warning", Subject: b.Name.String(), Owner: b.DisplayOwner(), Time: now,
					Text: fmt.Sprintf("Branch %s expires in %s (at %s). Extend it if it is still in use.", b.Name, short(b.ExpiresAt.Sub(now)), b.ExpiresAt.Format(time.RFC3339))})
			}
		}
	}
	ops, err := s.State.ListOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.State != operation.Failed || op.UpdatedAt.Before(s.Since) || !s.once("op/"+op.ID) {
			continue
		}
		notices = append(notices, Notice{Event: "operation.failed", Level: "error", Subject: op.Target.Name, Time: now,
			Text: fmt.Sprintf("%s of %s %s failed on %s: %s", op.Kind, op.Target.Resource, op.Target.Name, op.Node, op.Error)})
	}
	if s.DownAfter > 0 {
		nodes, err := s.State.ListNodes(ctx)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.down == nil {
			s.down = map[string]bool{}
		}
		for _, n := range nodes {
			isDown := n.Down(now, s.DownAfter)
			switch {
			case isDown && !s.down[n.Name]:
				notices = append(notices, Notice{Event: "node.down", Level: "error", Subject: n.Name, Time: now,
					Text: fmt.Sprintf("Node %s stopped answering (last seen %s). Its branches are unreachable.", n.Name, n.LastSeen.Format(time.RFC3339))})
			case !isDown && s.down[n.Name]:
				notices = append(notices, Notice{Event: "node.up", Level: "info", Subject: n.Name, Time: now, Text: fmt.Sprintf("Node %s is back.", n.Name)})
			}
			s.down[n.Name] = isDown
		}
		s.mu.Unlock()
	}
	return s.send(ctx, notices)
}

// Publish sends one notice that does not come from the cluster state (for
// example, about the licence) to every sender.
func (s *Service) Publish(ctx context.Context, event, level, subject, text string) error {
	return s.send(ctx, []Notice{{Event: event, Level: level, Subject: subject, Text: text, Time: s.Clock()}})
}

func (s *Service) send(ctx context.Context, notices []Notice) error {
	var errs []error
	for _, n := range notices {
		for _, snd := range s.Senders {
			if err := snd.Send(ctx, n); err != nil {
				errs = append(errs, fmt.Errorf("sending %s: %w", n.Event, err))
			}
		}
	}
	return errors.Join(errs...)
}

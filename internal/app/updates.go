package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/apt"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/update"
)

// updateAutoCheck follows the update.auto_check setting.
var updateAutoCheck atomic.Bool

// raftMembers is the cluster's Raft membership as the update use case sees it.
type raftMembers struct{ s *raftstore.Store }

func (m raftMembers) Leader() string  { id, _ := m.s.Leader(); return id }
func (m raftMembers) Voters() int     { return m.s.Voters() }
func (m raftMembers) Transfer() error { return m.s.TransferLeadership() }

// wireUpdates builds the update service and runs its loop. It returns nil
// when the state store cannot keep rollouts.
func wireUpdates(ctx context.Context, store any, cl *clusterRuntime, node string, bus *operation.Bus, log *slog.Logger) *update.Service {
	us, ok := store.(domain.UpdateStore)
	ms, ok2 := store.(domain.ModuleStore)
	if !ok || !ok2 {
		return nil
	}
	s := &update.Service{
		Store: us, Modules: ms, Packages: apt.Packages{Run: execx.Runner{}}, Self: node, Clock: time.Now,
		DownAfter: NodeDownAfter,
		Nodes: func(context.Context) ([]domain.Node, error) {
			return []domain.Node{{Name: node, Version: version, LastSeen: time.Now()}}, nil
		},
	}
	if cl != nil {
		s.Cluster = raftMembers{cl.store}
		s.Nodes = cl.store.ListNodes
	} else {
		// Without Raft nobody else shows the change: tell the console here.
		s.Changed = func() { bus.Publish("update.updated", map[string]string{"what": "rollout"}) }
	}
	go func() {
		last := ""
		for {
			err := s.Tick(ctx)
			if err == nil {
				err = s.CheckDue(ctx, updateAutoCheck.Load())
			}
			if msg := errString(err); msg != last {
				if err != nil {
					log.Warn("updating the cluster", "err", err)
				}
				last = msg
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

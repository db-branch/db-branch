package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

// hostService is what the node's branch service offers modules beyond Branches.
type hostService interface {
	RestoreCheckpointWith(ctx context.Context, n domain.Name, name, note string, report dbbranch.Reporter) (domain.Branch, error)
	BranchAdmin(ctx context.Context, n domain.Name) (domain.AdminAccess, error)
	OpenLine(ctx context.Context, n domain.Name, line int, checkpoint string) (string, error)
	CloseLine(ctx context.Context, dir string) error
}

// ModuleOpTimeout is how long an operation a module started may stay open.
var ModuleOpTimeout = time.Hour

// BranchHost is what the agent lets modules do with this node's branches.
func (s *Server) BranchHost() ext.BranchHost { return &branchHost{s: s, ops: map[string]*moduleOp{}} }

type moduleOp struct {
	ready  chan struct{}
	report func(string)
	done   chan string
}

type branchHost struct {
	s   *Server
	mu  sync.Mutex
	ops map[string]*moduleOp
}

func (h *branchHost) service() (Branches, hostService, error) {
	svc, err := h.s.Branches()
	if err != nil {
		return nil, nil, err
	}
	hs, ok := svc.(hostService)
	if !ok {
		return nil, nil, fmt.Errorf("%w: this node's storage cannot do that", domain.ErrConflict)
	}
	return svc, hs, nil
}

// local finds a branch this node holds.
func (h *branchHost) local(ctx context.Context, name string) (Branches, hostService, domain.Branch, error) {
	svc, hs, err := h.service()
	if err != nil {
		return nil, nil, domain.Branch{}, err
	}
	n, err := domain.ParseName(name)
	if err != nil {
		return nil, nil, domain.Branch{}, err
	}
	b, err := svc.GetBranch(ctx, n)
	if err != nil {
		return nil, nil, b, err
	}
	if h.s.remote(b.Node) {
		return nil, nil, b, fmt.Errorf("%w: branch %s is on node %s", dbbranch.ErrOtherNode, name, b.Node)
	}
	return svc, hs, b, nil
}

func stateOf(b domain.Branch) ext.BranchState {
	return ext.BranchState{Name: b.Name.String(), Owner: b.Owner, Engine: string(b.Runtime.Engine), Version: b.Runtime.Version,
		Status: string(b.Status), Record: b.Record, Anonymous: b.Owner == Anonymous.ID, CreatedAt: b.CreatedAt,
		Base: b.Base.String(), Snapshot: b.Snapshot}
}

func (h *branchHost) Branch(ctx context.Context, name string) (ext.BranchState, error) {
	_, _, b, err := h.local(ctx, name)
	if err != nil {
		return ext.BranchState{}, err
	}
	return stateOf(b), nil
}

func (h *branchHost) Branches(ctx context.Context) ([]ext.BranchState, error) {
	svc, _, err := h.service()
	if err != nil {
		return nil, err
	}
	list, err := svc.ListBranches(ctx)
	out := []ext.BranchState{}
	for _, b := range list {
		if !h.s.remote(b.Node) {
			out = append(out, stateOf(b))
		}
	}
	return out, err
}

func (h *branchHost) Graph(ctx context.Context, name string) (ext.CheckpointGraph, error) {
	svc, _, b, err := h.local(ctx, name)
	if err != nil {
		return ext.CheckpointGraph{}, err
	}
	g, err := svc.CheckpointGraph(ctx, b.Name)
	if err != nil {
		return ext.CheckpointGraph{}, err
	}
	out := ext.CheckpointGraph{Live: g.Live, Head: g.Head, Checkpoints: []ext.CheckpointInfo{}, Lines: []ext.LineInfo{}}
	for _, c := range g.Checkpoints {
		out.Checkpoints = append(out.Checkpoints, ext.CheckpointInfo{Name: c.Name, CreatedAt: c.CreatedAt, Line: c.Line, Auto: c.Auto, Current: c.Current})
	}
	for _, l := range g.Lines {
		li := ext.LineInfo{Line: l.Line, Live: l.Live, Parent: ext.NoLine, From: l.From}
		if l.Parent != nil {
			li.Parent = *l.Parent
		}
		out.Lines = append(out.Lines, li)
	}
	return out, nil
}

func (h *branchHost) CreateCheckpoint(ctx context.Context, name, checkpoint string) (ext.CheckpointInfo, error) {
	svc, _, b, err := h.local(ctx, name)
	if err != nil {
		return ext.CheckpointInfo{}, err
	}
	c, err := svc.CreateCheckpoint(ctx, b.Name, checkpoint)
	if err != nil {
		return ext.CheckpointInfo{}, err
	}
	return ext.CheckpointInfo{Name: c.Name, CreatedAt: c.CreatedAt, Line: c.Line, Auto: c.Auto, Current: c.Current}, nil
}

func (h *branchHost) RestoreCheckpoint(ctx context.Context, name, checkpoint, note string) error {
	svc, hs, b, err := h.local(ctx, name)
	if err != nil {
		return err
	}
	if checkpoint != domain.ResetPoint {
		if _, err := domain.ParseCheckpointName(checkpoint); err != nil {
			return err
		}
	}
	if err := b.CanCheckpoint(); err != nil {
		return err
	}
	_, err = hs.RestoreCheckpointWith(ctx, b.Name, checkpoint, note, nil)
	h.s.publishBranch(ctx, svc, b.Name)
	return err
}

func (h *branchHost) DB(ctx context.Context, name string) (ext.DBAccess, error) {
	_, hs, b, err := h.local(ctx, name)
	if err != nil {
		return ext.DBAccess{}, err
	}
	a, err := hs.BranchAdmin(ctx, b.Name)
	return dbAccess(a), err
}

// dbAccess is the module's view of an admin access (the maps are copied).
func dbAccess(a domain.AdminAccess) ext.DBAccess {
	out := ext.DBAccess{Engine: string(a.Engine), Host: a.Host, Socket: a.Socket, Port: a.Port, User: a.User, BinDir: a.BinDir, LibDir: a.LibDir}
	if len(a.Endpoints) > 0 {
		out.Endpoints = make(map[string]ext.Endpoint, len(a.Endpoints))
		for k, e := range a.Endpoints {
			out.Endpoints[k] = ext.Endpoint{Host: e.Host, Port: e.Port, Socket: e.Socket}
		}
	}
	if len(a.Env) > 0 {
		out.Env = make(map[string]string, len(a.Env))
		for k, v := range a.Env {
			out.Env[k] = v
		}
	}
	return out
}

func (h *branchHost) OpenLine(ctx context.Context, name string, line int, checkpoint string) (string, error) {
	_, hs, b, err := h.local(ctx, name)
	if err != nil {
		return "", err
	}
	return hs.OpenLine(ctx, b.Name, line, checkpoint)
}

func (h *branchHost) CloseLine(ctx context.Context, dir string) error {
	_, hs, err := h.service()
	if err != nil {
		return err
	}
	return hs.CloseLine(ctx, dir)
}

// StartOperation records an operation that stays open until the module ends it.
func (h *branchHost) StartOperation(ctx context.Context, kind, branch string) (string, error) {
	_, _, b, err := h.local(ctx, branch)
	if err != nil {
		return "", err
	}
	if kind == "" || len(kind) > 64 {
		return "", fmt.Errorf("%w: operation kind %q", domain.ErrInvalidName, kind)
	}
	mo := &moduleOp{ready: make(chan struct{}), done: make(chan string, 1)}
	op, err := h.s.Ops.StartAs(b.Owner, kind, operation.Target{Resource: "branch", Name: b.Name.String()},
		func(_ context.Context, report func(string)) error {
			mo.report = report
			close(mo.ready)
			select {
			case failure := <-mo.done:
				if failure != "" {
					return errors.New(failure)
				}
				return nil
			case <-time.After(ModuleOpTimeout):
				return errors.New("the module did not finish this operation")
			}
		})
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.ops[op.ID] = mo
	h.mu.Unlock()
	return op.ID, nil
}

func (h *branchHost) op(id string) (*moduleOp, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	mo, ok := h.ops[id]
	if !ok {
		return nil, fmt.Errorf("%w: operation %s was not started by a module (or is over)", operation.ErrNotFound, id)
	}
	return mo, nil
}

func (h *branchHost) ReportOperation(_ context.Context, id, phase string) error {
	mo, err := h.op(id)
	if err != nil {
		return err
	}
	<-mo.ready
	mo.report(phase)
	return nil
}

func (h *branchHost) EndOperation(ctx context.Context, id, failure string) error {
	mo, err := h.op(id)
	if err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.ops, id)
	h.mu.Unlock()
	mo.done <- failure
	// Tell the console once the operation has really ended.
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if op, err := h.s.Ops.Get(id); err == nil && op.State != operation.Running {
			h.s.PublishBranch(ctx, domainName(op.Target.Name))
			break
		}
	}
	return nil
}

func domainName(s string) domain.Name {
	n, _ := domain.ParseName(s)
	return n
}

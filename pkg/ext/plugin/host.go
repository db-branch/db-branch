package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/rpc"

	"github.com/db-branch/db-branch/pkg/ext"
)

// ServerOptionsReply is the RPC reply of ServerOptions.
type ServerOptionsReply struct {
	Options ext.ServerOptions
	Err     RPCError
}

func (s *rpcServer) ServerOptions(b ext.ServerBranch, out *ServerOptionsReply) error {
	if s.m.ServerOptioner != nil {
		o, err := s.m.ServerOptioner.ServerOptions(context.Background(), b)
		out.Options, out.Err = o, toRPC(err)
	}
	return nil
}

// AfterRestoreArgs are the RPC arguments of AfterRestore.
type AfterRestoreArgs struct {
	Branch ext.ServerBranch
	Note   string
}

func (s *rpcServer) AfterRestore(a AfterRestoreArgs, out *RPCError) error {
	if s.m.RestoreFinisher != nil {
		*out = toRPC(s.m.RestoreFinisher.AfterRestore(context.Background(), a.Branch, a.Note))
	}
	return nil
}

// ServerOptions implements ext.ServerOptioner for a module that has one.
func (r *Remote) ServerOptions(_ context.Context, b ext.ServerBranch) (ext.ServerOptions, error) {
	var out ServerOptionsReply
	if err := r.c.Call("Plugin.ServerOptions", b, &out); err != nil {
		return ext.ServerOptions{}, err
	}
	return out.Options, out.Err.err()
}

// AfterRestore implements ext.RestoreFinisher for a module that has one.
func (r *Remote) AfterRestore(_ context.Context, b ext.ServerBranch, note string) error {
	var out RPCError
	if err := r.c.Call("Plugin.AfterRestore", AfterRestoreArgs{Branch: b, Note: note}, &out); err != nil {
		return err
	}
	return out.err()
}

// HostCall is one BranchHost operation over RPC (Op names the method).
type HostCall struct {
	Op                                          string
	Name, Checkpoint, Note, Kind, Dir, ID, Text string
	Line                                        int
}

// HostReply is its result.
type HostReply struct {
	State      ext.BranchState
	States     []ext.BranchState
	Graph      ext.CheckpointGraph
	Checkpoint ext.CheckpointInfo
	DB         ext.DBAccess
	Text       string
	Err        RPCError
}

// BranchHostServer serves the agent's BranchHost to a module (agent side).
type BranchHostServer struct{ H ext.BranchHost }

// Call runs one operation.
func (s *BranchHostServer) Call(c HostCall, out *HostReply) error {
	ctx, h := context.Background(), s.H
	var err error
	switch c.Op {
	case "branch":
		out.State, err = h.Branch(ctx, c.Name)
	case "branches":
		out.States, err = h.Branches(ctx)
	case "graph":
		out.Graph, err = h.Graph(ctx, c.Name)
	case "checkpoint":
		out.Checkpoint, err = h.CreateCheckpoint(ctx, c.Name, c.Checkpoint)
	case "restore":
		err = h.RestoreCheckpoint(ctx, c.Name, c.Checkpoint, c.Note)
	case "reset":
		err = h.Reset(ctx, c.Name, c.Note)
	case "db":
		out.DB, err = h.DB(ctx, c.Name)
	case "open-line":
		out.Text, err = h.OpenLine(ctx, c.Name, c.Line, c.Checkpoint)
	case "close-line":
		err = h.CloseLine(ctx, c.Dir)
	case "start-op":
		out.Text, err = h.StartOperation(ctx, c.Kind, c.Name)
	case "report-op":
		err = h.ReportOperation(ctx, c.ID, c.Text)
	case "end-op":
		err = h.EndOperation(ctx, c.ID, c.Text)
	default:
		err = fmt.Errorf("unknown branch host operation %q", c.Op)
	}
	out.Err = toRPC(err)
	return nil
}

// hostRemote is the agent's BranchHost seen from the module.
type hostRemote struct{ c *rpc.Client }

func (h *hostRemote) call(c HostCall) (HostReply, error) {
	var out HostReply
	if err := h.c.Call("Plugin.Call", c, &out); err != nil {
		return out, errors.Join(errors.New("the agent did not answer"), err)
	}
	return out, out.Err.err()
}

func (h *hostRemote) Branch(_ context.Context, name string) (ext.BranchState, error) {
	out, err := h.call(HostCall{Op: "branch", Name: name})
	return out.State, err
}

func (h *hostRemote) Branches(context.Context) ([]ext.BranchState, error) {
	out, err := h.call(HostCall{Op: "branches"})
	return out.States, err
}

func (h *hostRemote) Graph(_ context.Context, name string) (ext.CheckpointGraph, error) {
	out, err := h.call(HostCall{Op: "graph", Name: name})
	return out.Graph, err
}

func (h *hostRemote) CreateCheckpoint(_ context.Context, name, checkpoint string) (ext.CheckpointInfo, error) {
	out, err := h.call(HostCall{Op: "checkpoint", Name: name, Checkpoint: checkpoint})
	return out.Checkpoint, err
}

func (h *hostRemote) RestoreCheckpoint(_ context.Context, name, checkpoint, note string) error {
	_, err := h.call(HostCall{Op: "restore", Name: name, Checkpoint: checkpoint, Note: note})
	return err
}

func (h *hostRemote) Reset(_ context.Context, name, note string) error {
	_, err := h.call(HostCall{Op: "reset", Name: name, Note: note})
	return err
}

func (h *hostRemote) DB(_ context.Context, name string) (ext.DBAccess, error) {
	out, err := h.call(HostCall{Op: "db", Name: name})
	return out.DB, err
}

func (h *hostRemote) OpenLine(_ context.Context, name string, line int, checkpoint string) (string, error) {
	out, err := h.call(HostCall{Op: "open-line", Name: name, Line: line, Checkpoint: checkpoint})
	return out.Text, err
}

func (h *hostRemote) CloseLine(_ context.Context, dir string) error {
	_, err := h.call(HostCall{Op: "close-line", Dir: dir})
	return err
}

func (h *hostRemote) StartOperation(_ context.Context, kind, branch string) (string, error) {
	out, err := h.call(HostCall{Op: "start-op", Kind: kind, Name: branch})
	return out.Text, err
}

func (h *hostRemote) ReportOperation(_ context.Context, id, phase string) error {
	_, err := h.call(HostCall{Op: "report-op", ID: id, Text: phase})
	return err
}

func (h *hostRemote) EndOperation(_ context.Context, id, failure string) error {
	_, err := h.call(HostCall{Op: "end-op", ID: id, Text: failure})
	return err
}

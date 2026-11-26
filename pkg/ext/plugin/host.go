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
	Principal                                   ext.Principal
	Owner                                       string
}

// HostReply is its result.
type HostReply struct {
	State      ext.BranchState
	States     []ext.BranchState
	Graph      ext.CheckpointGraph
	Checkpoint ext.CheckpointInfo
	DB         ext.DBAccess
	Text       string
	Allowed    bool
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
	case "shares-branches":
		if sh, ok := h.(ext.BranchSharer); ok {
			out.Allowed = sh.SharesBranches(ctx, c.Principal, c.Owner)
		}
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

// SharesBranches implements ext.BranchSharer over RPC.
func (h *hostRemote) SharesBranches(_ context.Context, p ext.Principal, owner string) bool {
	out, err := h.call(HostCall{Op: "shares-branches", Principal: p, Owner: owner})
	return err == nil && out.Allowed
}

// UserHostCall is one UserStore operation over RPC.
type UserHostCall struct {
	Op                       string
	ID, Client, Username, PW string
	Create                   ext.UserCreate
	Update                   ext.UserUpdate
}

// UserHostReply is its result.
type UserHostReply struct {
	User  ext.User
	Users []ext.User
	Err   RPCError
}

// UserHostServer serves the agent's UserStore to a module (agent side).
type UserHostServer struct{ U ext.UserStore }

// Call runs one UserStore operation.
func (s *UserHostServer) Call(c UserHostCall, out *UserHostReply) error {
	ctx, u := context.Background(), s.U
	var err error
	switch c.Op {
	case "list":
		out.Users, err = u.ListUsers(ctx)
	case "get":
		out.User, err = u.GetUser(ctx, c.ID)
	case "create":
		out.User, err = u.CreateUser(ctx, c.Create)
	case "update":
		out.User, err = u.UpdateUser(ctx, c.ID, c.Update)
	case "delete":
		err = u.DeleteUser(ctx, c.ID)
	case "verify":
		out.User, err = u.VerifyPassword(ctx, c.Client, c.Username, c.PW)
	default:
		err = fmt.Errorf("unknown user host operation %q", c.Op)
	}
	out.Err = toRPC(err)
	return nil
}

type userRemote struct{ c *rpc.Client }

func (u *userRemote) call(c UserHostCall) (UserHostReply, error) {
	var out UserHostReply
	if err := u.c.Call("Plugin.Call", c, &out); err != nil {
		return out, errors.Join(errors.New("the agent did not answer"), err)
	}
	return out, out.Err.err()
}

func (u *userRemote) ListUsers(context.Context) ([]ext.User, error) {
	out, err := u.call(UserHostCall{Op: "list"})
	return out.Users, err
}

func (u *userRemote) GetUser(_ context.Context, id string) (ext.User, error) {
	out, err := u.call(UserHostCall{Op: "get", ID: id})
	return out.User, err
}

func (u *userRemote) CreateUser(_ context.Context, in ext.UserCreate) (ext.User, error) {
	out, err := u.call(UserHostCall{Op: "create", Create: in})
	return out.User, err
}

func (u *userRemote) UpdateUser(_ context.Context, id string, in ext.UserUpdate) (ext.User, error) {
	out, err := u.call(UserHostCall{Op: "update", ID: id, Update: in})
	return out.User, err
}

func (u *userRemote) DeleteUser(_ context.Context, id string) error {
	_, err := u.call(UserHostCall{Op: "delete", ID: id})
	return err
}

func (u *userRemote) VerifyPassword(_ context.Context, client, username, password string) (ext.User, error) {
	out, err := u.call(UserHostCall{Op: "verify", Client: client, Username: username, PW: password})
	return out.User, err
}

// Package plugin runs db-branch modules as separate processes and talks to
// them over RPC (hashicorp/go-plugin), so a module ships as its own binary
// and package and the OSS agent stays unchanged.
//
// A module's main:
//
//	func main() {
//		plugin.Serve(ext.Module{Name: "audit-file", Version: "1.0.0", AuditSink: mySink{}})
//	}
//
// The agent loads every module binary in /usr/lib/db-branch/modules at start.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"golang.org/x/sys/unix"

	"github.com/db-branch/db-branch/pkg/ext"
)

// Handshake keeps the agent from running arbitrary binaries as modules (and
// modules from being started by hand). Bump ProtocolVersion on breaking
// changes to the RPC below.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "DB_BRANCH_MODULE",
	MagicCookieValue: "8f2c1d0e6b7a4c39a5e1d2f3b4c5a6e7",
}

const pluginName = "module"

// Info describes a loaded module.
type Info struct {
	Name, Version                                         string
	Authenticator, Authorizer, Sink, Router, Configurable bool
	Handler, Starter                                      bool
	// PasswordChanger: the Authenticator's users may change their password.
	PasswordChanger   bool
	PrincipalResolver bool
	// RequiresLicense: the module needs a licence key.
	RequiresLicense    bool
	PublicPaths        []string
	LoginMethods       []ext.LoginMethod
	BranchPolicy       bool
	BranchExtendPolicy bool
	BranchTTLCap       bool
	// Storage: the module provides storage; SharedStorage: seen by every node.
	Storage, SharedStorage bool
	Masker, MaskPolicy     bool
	// RefreshFollower: the module says which mask runs follow a refresh.
	RefreshFollower bool
	// ServerOptioner adds options to branch servers; RestoreFinisher works
	// on a branch after a restore it started.
	ServerOptioner, RestoreFinisher bool
	Ports                           map[string]int
	Settings                        []ext.Setting
	Pages                           []ext.Page
}

// Serve runs m until the agent stops it. Call it from the module's main.
func Serve(m ext.Module) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         map[string]goplugin.Plugin{pluginName: &modulePlugin{impl: m}},
	})
}

type modulePlugin struct{ impl ext.Module }

func (p *modulePlugin) Server(b *goplugin.MuxBroker) (any, error) {
	return &rpcServer{m: p.impl, broker: b}, nil
}
func (p *modulePlugin) Client(b *goplugin.MuxBroker, c *rpc.Client) (any, error) {
	return &Remote{c: c, broker: b}, nil
}

// RPCError carries an error across the process boundary as text plus which
// sentinel it wraps (net/rpc needs exported types).
type RPCError struct {
	Msg            string
	Unauth, Forbid bool
}

func toRPC(err error) RPCError {
	if err == nil {
		return RPCError{}
	}
	return RPCError{Msg: err.Error(), Unauth: errors.Is(err, ext.ErrUnauthenticated), Forbid: errors.Is(err, ext.ErrForbidden)}
}

func (e RPCError) err() error {
	switch {
	case e.Msg == "":
		return nil
	case e.Unauth:
		return fmt.Errorf("%w: %s", ext.ErrUnauthenticated, strings.TrimPrefix(e.Msg, ext.ErrUnauthenticated.Error()+": "))
	case e.Forbid:
		return fmt.Errorf("%w: %s", ext.ErrForbidden, strings.TrimPrefix(e.Msg, ext.ErrForbidden.Error()+": "))
	}
	return errors.New(e.Msg)
}

type rpcServer struct {
	m      ext.Module
	broker *goplugin.MuxBroker
}

// AuthenticateReply is the RPC reply of Authenticate.
type AuthenticateReply struct {
	Principal ext.Principal
	Err       RPCError
}

// AuthorizeArgs are the RPC arguments of Authorize.
type AuthorizeArgs struct {
	Principal ext.Principal
	Action    string
	Resource  ext.Resource
}

func (s *rpcServer) Info(_ struct{}, out *Info) error {
	*out = Info{Name: s.m.Name, Version: s.m.Version,
		Authenticator: s.m.Authenticator != nil, Authorizer: s.m.Authorizer != nil, Sink: s.m.AuditSink != nil,
		PasswordChanger: s.passwordChanger() != nil, PrincipalResolver: s.m.PrincipalResolver != nil,
		RequiresLicense: s.m.RequiresLicense, PublicPaths: s.m.PublicPaths, LoginMethods: s.m.LoginMethods,
		BranchPolicy: s.m.BranchPolicy != nil, BranchExtendPolicy: s.branchExtendPolicy() != nil,
		BranchTTLCap: s.branchTTLCap() != nil,
		Router: s.m.Router != nil, Ports: s.m.Ports, Configurable: s.m.Configurable != nil, Settings: s.m.Settings,
		Handler: s.m.Handler != nil, Pages: s.m.Pages, Starter: s.m.Starter != nil,
		Storage: s.m.Storage != nil, SharedStorage: s.m.SharedStorage,
		Masker: s.m.Masker != nil, MaskPolicy: s.m.MaskPolicy != nil, RefreshFollower: s.m.RefreshFollower != nil,
		ServerOptioner: s.m.ServerOptioner != nil, RestoreFinisher: s.m.RestoreFinisher != nil}
	return nil
}

// ServeReply is the RPC reply of Serve.
type ServeReply struct {
	Response ext.Response
	Err      RPCError
}

func (s *rpcServer) Serve(r ext.Request, out *ServeReply) error {
	if s.m.Handler == nil {
		out.Response = ext.Response{Status: 404, ContentType: "text/plain", Body: []byte("this module has no API")}
		return nil
	}
	resp, err := s.m.Handler.Serve(context.Background(), r)
	out.Response, out.Err = resp, toRPC(err)
	return nil
}

// StartArgs are the RPC arguments of Start: the host, with the broker
// stream on which the agent serves the module's Store.
type StartArgs struct {
	Node, DataDir string
	StoreID       uint32
	// BranchesID is the broker stream of the agent's BranchHost (0: none).
	BranchesID uint32
	// APIID is the broker stream of the agent's ModuleAPI (0: none).
	APIID uint32
}

func (s *rpcServer) Start(a StartArgs, out *RPCError) error {
	if s.m.Starter == nil {
		return nil
	}
	conn, err := s.broker.Dial(a.StoreID)
	if err != nil {
		*out = toRPC(err)
		return nil
	}
	h := ext.Host{Node: a.Node, DataDir: a.DataDir, Store: &remoteStore{c: rpc.NewClient(conn)}}
	if a.BranchesID != 0 {
		bconn, err := s.broker.Dial(a.BranchesID)
		if err != nil {
			*out = toRPC(err)
			return nil
		}
		h.Branches = &hostRemote{c: rpc.NewClient(bconn)}
	}
	if a.APIID != 0 {
		aconn, err := s.broker.Dial(a.APIID)
		if err != nil {
			*out = toRPC(err)
			return nil
		}
		h.API = &apiRemote{c: rpc.NewClient(aconn)}
	}
	*out = toRPC(s.m.Starter.Start(context.Background(), h))
	return nil
}

// remoteStore is the agent's Store seen from the module.
type remoteStore struct{ c *rpc.Client }

// StoreListReply is the RPC reply of Store.List.
type StoreListReply struct {
	Records map[string][]byte
	Err     RPCError
}

// StorePutArgs are the RPC arguments of Store.Put and Store.Delete.
type StorePutArgs struct {
	Key   string
	Value []byte
}

func (r *remoteStore) List(context.Context) (map[string][]byte, error) {
	var out StoreListReply
	if err := r.c.Call("Plugin.List", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Records, out.Err.err()
}

func (r *remoteStore) Put(_ context.Context, key string, value []byte) error {
	var out RPCError
	if err := r.c.Call("Plugin.Put", StorePutArgs{Key: key, Value: value}, &out); err != nil {
		return err
	}
	return out.err()
}

func (r *remoteStore) Delete(_ context.Context, key string) error {
	var out RPCError
	if err := r.c.Call("Plugin.Delete", StorePutArgs{Key: key}, &out); err != nil {
		return err
	}
	return out.err()
}

// StoreServer serves a module's Store to it (the agent side).
type StoreServer struct{ S ext.Store }

func (s *StoreServer) List(_ struct{}, out *StoreListReply) error {
	recs, err := s.S.List(context.Background())
	out.Records, out.Err = recs, toRPC(err)
	return nil
}

func (s *StoreServer) Put(a StorePutArgs, out *RPCError) error {
	*out = toRPC(s.S.Put(context.Background(), a.Key, a.Value))
	return nil
}

func (s *StoreServer) Delete(a StorePutArgs, out *RPCError) error {
	*out = toRPC(s.S.Delete(context.Background(), a.Key))
	return nil
}

func (s *rpcServer) Authenticate(c ext.Credentials, out *AuthenticateReply) error {
	if s.m.Authenticator == nil {
		out.Err = toRPC(ext.ErrUnauthenticated)
		return nil
	}
	p, err := s.m.Authenticator.Authenticate(context.Background(), c)
	out.Principal, out.Err = p, toRPC(err)
	return nil
}

func (s *rpcServer) passwordChanger() ext.PasswordChanger {
	pc, _ := s.m.Authenticator.(ext.PasswordChanger)
	return pc
}

func (s *rpcServer) branchExtendPolicy() ext.BranchExtendPolicy {
	ep, _ := s.m.BranchPolicy.(ext.BranchExtendPolicy)
	return ep
}

func (s *rpcServer) branchTTLCap() ext.BranchTTLCap {
	tc, _ := s.m.BranchPolicy.(ext.BranchTTLCap)
	return tc
}

// BranchTTLCapReply is the RPC result of MaxBranchTTL.
type BranchTTLCapReply struct {
	TTL    time.Duration
	Capped bool
}

// ResolvePrincipal resolves a Principal via the module's PrincipalResolver.
func (s *rpcServer) ResolvePrincipal(p ext.Principal, out *AuthenticateReply) error {
	if s.m.PrincipalResolver == nil {
		out.Principal = p
		return nil
	}
	next, err := s.m.PrincipalResolver.ResolvePrincipal(context.Background(), p)
	out.Principal, out.Err = next, toRPC(err)
	return nil
}

// ChangePasswordArgs are the RPC arguments of ChangePassword.
type ChangePasswordArgs struct {
	Principal     ext.Principal
	Current, Next string
}

func (s *rpcServer) ChangePassword(a ChangePasswordArgs, out *RPCError) error {
	pc := s.passwordChanger()
	if pc == nil {
		*out = toRPC(ext.ErrUnauthenticated)
		return nil
	}
	*out = toRPC(pc.ChangePassword(context.Background(), a.Principal, a.Current, a.Next))
	return nil
}

// BranchArgs are the RPC arguments of CheckBranch and SharesBranches.
type BranchArgs struct {
	Principal ext.Principal
	Request   ext.BranchRequest
	Owner     string
}

func (s *rpcServer) CheckBranch(a BranchArgs, out *RPCError) error {
	if s.m.BranchPolicy != nil {
		*out = toRPC(s.m.BranchPolicy.CheckBranch(context.Background(), a.Principal, a.Request))
	}
	return nil
}

func (s *rpcServer) SharesBranches(a BranchArgs, out *bool) error {
	*out = s.m.BranchPolicy != nil && s.m.BranchPolicy.SharesBranches(context.Background(), a.Principal, a.Owner)
	return nil
}

// CheckExtend runs CheckExtend when the module's BranchPolicy implements BranchExtendPolicy.
func (s *rpcServer) CheckExtend(a BranchArgs, out *RPCError) error {
	if ep := s.branchExtendPolicy(); ep != nil {
		*out = toRPC(ep.CheckExtend(context.Background(), a.Principal, a.Request))
	}
	return nil
}

// MaxBranchTTL runs MaxBranchTTL when the module's BranchPolicy implements BranchTTLCap.
func (s *rpcServer) MaxBranchTTL(a BranchArgs, out *BranchTTLCapReply) error {
	if tc := s.branchTTLCap(); tc != nil {
		out.TTL, out.Capped = tc.MaxBranchTTL(context.Background(), a.Principal, a.Request.Base)
	}
	return nil
}

func (s *rpcServer) Authorize(a AuthorizeArgs, out *RPCError) error {
	if s.m.Authorizer == nil {
		return nil
	}
	*out = toRPC(s.m.Authorizer.Authorize(context.Background(), a.Principal, a.Action, a.Resource))
	return nil
}

func (s *rpcServer) Record(e ext.Event, out *RPCError) error {
	if s.m.AuditSink == nil {
		return nil
	}
	*out = toRPC(s.m.AuditSink.Record(context.Background(), e))
	return nil
}

func (s *rpcServer) Configure(values map[string]string, out *RPCError) error {
	if s.m.Configurable == nil {
		return nil
	}
	*out = toRPC(s.m.Configurable.Configure(context.Background(), values))
	return nil
}

func (s *rpcServer) Route(c ext.RouteConfig, out *RPCError) error {
	if s.m.Router == nil {
		return nil
	}
	*out = toRPC(s.m.Router.Route(context.Background(), c))
	return nil
}

// MaskReply is the RPC reply of Mask and PrepareMask.
type MaskReply struct {
	Text string
	Err  RPCError
}

// MaskRecordArgs are MaskRecorded's arguments.
type MaskRecordArgs struct {
	Plan     ext.MaskPlan
	Snapshot string
}

func (s *rpcServer) Mask(j ext.MaskJob, out *MaskReply) error {
	if s.m.Masker == nil {
		out.Err = toRPC(errors.New("this module is not a masker"))
		return nil
	}
	log, err := s.m.Masker.Mask(context.Background(), j)
	out.Text, out.Err = log, toRPC(err)
	return nil
}

func (s *rpcServer) PrepareMask(p ext.MaskPlan, out *MaskReply) error {
	out.Text = p.Rules
	if s.m.MaskPolicy != nil {
		rules, err := s.m.MaskPolicy.PrepareMask(context.Background(), p)
		out.Text, out.Err = rules, toRPC(err)
	}
	return nil
}

func (s *rpcServer) VerifyMask(p ext.MaskPlan, out *RPCError) error {
	if s.m.MaskPolicy != nil {
		*out = toRPC(s.m.MaskPolicy.VerifyMask(context.Background(), p))
	}
	return nil
}

func (s *rpcServer) MaskRecorded(a MaskRecordArgs, out *RPCError) error {
	if s.m.MaskPolicy != nil {
		*out = toRPC(s.m.MaskPolicy.MaskRecorded(context.Background(), a.Plan, a.Snapshot))
	}
	return nil
}

func (r *Remote) Mask(_ context.Context, j ext.MaskJob) (string, error) {
	var out MaskReply
	if err := r.c.Call("Plugin.Mask", j, &out); err != nil {
		return "", err
	}
	return out.Text, out.Err.err()
}

func (r *Remote) PrepareMask(_ context.Context, p ext.MaskPlan) (string, error) {
	var out MaskReply
	if err := r.c.Call("Plugin.PrepareMask", p, &out); err != nil {
		return "", err
	}
	return out.Text, out.Err.err()
}

func (r *Remote) VerifyMask(_ context.Context, p ext.MaskPlan) error {
	var out RPCError
	if err := r.c.Call("Plugin.VerifyMask", p, &out); err != nil {
		return err
	}
	return out.err()
}

func (r *Remote) MaskRecorded(_ context.Context, p ext.MaskPlan, snapshot string) error {
	var out RPCError
	if err := r.c.Call("Plugin.MaskRecorded", MaskRecordArgs{Plan: p, Snapshot: snapshot}, &out); err != nil {
		return err
	}
	return out.err()
}

// AfterRefreshArgs are AfterRefresh's arguments.
type AfterRefreshArgs struct{ Base, Snapshot string }

// AfterRefreshReply is the RPC reply of AfterRefresh.
type AfterRefreshReply struct {
	Runs []ext.MaskRun
	Err  RPCError
}

func (s *rpcServer) AfterRefresh(a AfterRefreshArgs, out *AfterRefreshReply) error {
	if s.m.RefreshFollower != nil {
		runs, err := s.m.RefreshFollower.AfterRefresh(context.Background(), a.Base, a.Snapshot)
		out.Runs, out.Err = runs, toRPC(err)
	}
	return nil
}

func (r *Remote) AfterRefresh(_ context.Context, base, snapshot string) ([]ext.MaskRun, error) {
	var out AfterRefreshReply
	if err := r.c.Call("Plugin.AfterRefresh", AfterRefreshArgs{Base: base, Snapshot: snapshot}, &out); err != nil {
		return nil, err
	}
	return out.Runs, out.Err.err()
}

// StorageCall is one storage operation over RPC.
type StorageCall struct {
	Op, Dataset, Snapshot, Target string
	Props                         map[string]string
}

// StorageReply is its result.
type StorageReply struct {
	Exists   bool
	Info     ext.DatasetInfo
	Datasets []string
	Err      RPCError
}

func (s *rpcServer) Storage(c StorageCall, out *StorageReply) error {
	st, ctx := s.m.Storage, context.Background()
	if st == nil {
		out.Err = toRPC(errors.New("this module provides no storage"))
		return nil
	}
	var err error
	switch c.Op {
	case "create":
		err = st.Create(ctx, c.Dataset, c.Props)
	case "snapshot":
		err = st.Snapshot(ctx, c.Dataset, c.Snapshot)
	case "clone":
		err = st.Clone(ctx, c.Dataset, c.Snapshot, c.Target)
	case "rollback":
		err = st.Rollback(ctx, c.Dataset, c.Snapshot)
	case "destroy":
		err = st.Destroy(ctx, c.Dataset)
	case "destroy-snapshot":
		err = st.DestroySnapshot(ctx, c.Dataset, c.Snapshot)
	case "exists":
		out.Exists, err = st.Exists(ctx, c.Dataset)
	case "info":
		out.Info, err = st.Info(ctx, c.Dataset)
	case "mounted":
		out.Datasets, err = st.Mounted(ctx, c.Dataset)
	case "release":
		err = st.Release(ctx, c.Dataset)
	default:
		err = fmt.Errorf("unknown storage operation %q", c.Op)
	}
	out.Err = toRPC(err)
	return nil
}

// storageRemote is a module's Storage seen from the agent.
type storageRemote struct{ r *Remote }

func (s storageRemote) call(c StorageCall) (StorageReply, error) {
	var out StorageReply
	if err := s.r.c.Call("Plugin.Storage", c, &out); err != nil {
		return out, err
	}
	return out, out.Err.err()
}

func (s storageRemote) do(c StorageCall) error { _, err := s.call(c); return err }

func (s storageRemote) Create(_ context.Context, ds string, props map[string]string) error {
	return s.do(StorageCall{Op: "create", Dataset: ds, Props: props})
}
func (s storageRemote) Snapshot(_ context.Context, ds, snap string) error {
	return s.do(StorageCall{Op: "snapshot", Dataset: ds, Snapshot: snap})
}
func (s storageRemote) Clone(_ context.Context, ds, snap, target string) error {
	return s.do(StorageCall{Op: "clone", Dataset: ds, Snapshot: snap, Target: target})
}
func (s storageRemote) Rollback(_ context.Context, ds, snap string) error {
	return s.do(StorageCall{Op: "rollback", Dataset: ds, Snapshot: snap})
}
func (s storageRemote) Destroy(_ context.Context, ds string) error {
	return s.do(StorageCall{Op: "destroy", Dataset: ds})
}
func (s storageRemote) DestroySnapshot(_ context.Context, ds, snap string) error {
	return s.do(StorageCall{Op: "destroy-snapshot", Dataset: ds, Snapshot: snap})
}
func (s storageRemote) Exists(_ context.Context, ds string) (bool, error) {
	out, err := s.call(StorageCall{Op: "exists", Dataset: ds})
	return out.Exists, err
}
func (s storageRemote) Info(_ context.Context, ds string) (ext.DatasetInfo, error) {
	out, err := s.call(StorageCall{Op: "info", Dataset: ds})
	return out.Info, err
}
func (s storageRemote) Mounted(_ context.Context, parent string) ([]string, error) {
	out, err := s.call(StorageCall{Op: "mounted", Dataset: parent})
	return out.Datasets, err
}
func (s storageRemote) Release(_ context.Context, ds string) error {
	return s.do(StorageCall{Op: "release", Dataset: ds})
}

// Remote is a module seen from the agent. It implements every port; Info
// says which ones the module really provides.
type Remote struct {
	c      *rpc.Client
	broker *goplugin.MuxBroker
}

// Serve passes a request to the module's Handler.
func (r *Remote) Serve(_ context.Context, req ext.Request) (ext.Response, error) {
	var out ServeReply
	if err := r.c.Call("Plugin.Serve", req, &out); err != nil {
		return ext.Response{}, err
	}
	return out.Response, out.Err.err()
}

// Start gives the module its host; the agent serves h.Store to it.
func (r *Remote) Start(_ context.Context, h ext.Host) error {
	id := r.broker.NextId()
	go r.broker.AcceptAndServe(id, &StoreServer{S: h.Store})
	args := StartArgs{Node: h.Node, DataDir: h.DataDir, StoreID: id}
	if h.Branches != nil {
		args.BranchesID = r.broker.NextId()
		go r.broker.AcceptAndServe(args.BranchesID, &BranchHostServer{H: h.Branches})
	}
	if h.API != nil {
		args.APIID = r.broker.NextId()
		go r.broker.AcceptAndServe(args.APIID, &APIServer{A: h.API})
	}
	var out RPCError
	if err := r.c.Call("Plugin.Start", args, &out); err != nil {
		return err
	}
	return out.err()
}

// Info asks the module what it is.
func (r *Remote) Info() (Info, error) {
	var out Info
	return out, r.c.Call("Plugin.Info", struct{}{}, &out)
}

func (r *Remote) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	var out AuthenticateReply
	if err := r.c.Call("Plugin.Authenticate", c, &out); err != nil {
		return ext.Principal{}, err
	}
	return out.Principal, out.Err.err()
}

// passwordRemote is a remote Authenticator whose users change passwords.
type passwordRemote struct{ *Remote }

func (r passwordRemote) ChangePassword(_ context.Context, p ext.Principal, current, next string) error {
	var out RPCError
	if err := r.c.Call("Plugin.ChangePassword", ChangePasswordArgs{Principal: p, Current: current, Next: next}, &out); err != nil {
		return err
	}
	return out.err()
}

// ResolvePrincipal implements ext.PrincipalResolver over RPC.
func (r *Remote) ResolvePrincipal(_ context.Context, p ext.Principal) (ext.Principal, error) {
	var out AuthenticateReply
	if err := r.c.Call("Plugin.ResolvePrincipal", p, &out); err != nil {
		return ext.Principal{}, err
	}
	return out.Principal, out.Err.err()
}

// CheckBranch implements ext.BranchPolicy for a module that has one.
func (r *Remote) CheckBranch(_ context.Context, p ext.Principal, req ext.BranchRequest) error {
	var out RPCError
	if err := r.c.Call("Plugin.CheckBranch", BranchArgs{Principal: p, Request: req}, &out); err != nil {
		return err
	}
	return out.err()
}

// SharesBranches implements ext.BranchPolicy.
func (r *Remote) SharesBranches(_ context.Context, p ext.Principal, owner string) bool {
	var out bool
	return r.c.Call("Plugin.SharesBranches", BranchArgs{Principal: p, Owner: owner}, &out) == nil && out
}

type extendPolicyRemote struct{ *Remote }

func (r extendPolicyRemote) CheckExtend(_ context.Context, p ext.Principal, req ext.BranchRequest) error {
	var out RPCError
	if err := r.c.Call("Plugin.CheckExtend", BranchArgs{Principal: p, Request: req}, &out); err != nil {
		return err
	}
	return out.err()
}

type ttlCapPolicyRemote struct{ *Remote }

func (r ttlCapPolicyRemote) MaxBranchTTL(_ context.Context, p ext.Principal, base string) (time.Duration, bool) {
	var out BranchTTLCapReply
	if err := r.c.Call("Plugin.MaxBranchTTL", BranchArgs{Principal: p, Request: ext.BranchRequest{Base: base}}, &out); err != nil {
		return 0, false
	}
	return out.TTL, out.Capped
}

func remoteBranchPolicy(r *Remote, info Info) ext.BranchPolicy {
	switch {
	case info.BranchExtendPolicy && info.BranchTTLCap:
		return fullPolicyRemote{extendPolicyRemote{r}, ttlCapPolicyRemote{r}}
	case info.BranchExtendPolicy:
		return extendPolicyRemote{r}
	case info.BranchTTLCap:
		return ttlCapPolicyRemote{r}
	default:
		return r
	}
}

type fullPolicyRemote struct {
	extendPolicyRemote
	ttlCapPolicyRemote
}

func (r fullPolicyRemote) CheckBranch(ctx context.Context, p ext.Principal, req ext.BranchRequest) error {
	return r.extendPolicyRemote.CheckBranch(ctx, p, req)
}

func (r fullPolicyRemote) SharesBranches(ctx context.Context, p ext.Principal, owner string) bool {
	return r.extendPolicyRemote.SharesBranches(ctx, p, owner)
}

func (r *Remote) Authorize(_ context.Context, p ext.Principal, action string, res ext.Resource) error {
	var out RPCError
	if err := r.c.Call("Plugin.Authorize", AuthorizeArgs{Principal: p, Action: action, Resource: res}, &out); err != nil {
		return err
	}
	return out.err()
}

func (r *Remote) Record(_ context.Context, e ext.Event) error {
	var out RPCError
	if err := r.c.Call("Plugin.Record", e, &out); err != nil {
		return err
	}
	return out.err()
}

func (r *Remote) Configure(_ context.Context, values map[string]string) error {
	var out RPCError
	if err := r.c.Call("Plugin.Configure", values, &out); err != nil {
		return err
	}
	return out.err()
}

func (r *Remote) Route(_ context.Context, c ext.RouteConfig) error {
	var out RPCError
	if err := r.c.Call("Plugin.Route", c, &out); err != nil {
		return err
	}
	return out.err()
}

// Loaded is a running module.
type Loaded struct {
	Path   string
	Info   Info
	Module ext.Module
	client *goplugin.Client
}

// Close stops the module process.
func (l *Loaded) Close() { l.client.Kill() }

// ErrUntrusted is returned for a module file or directory that the agent must
// not execute: relative or traversal path, not owned by root, writable by
// group or others, a symbolic link, special mode bits, missing owner execute
// permission, or not the expected file type.
var ErrUntrusted = errors.New("module file is not trusted")

func fileOwner(fi fs.FileInfo) (uint32, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return st.Uid, true
	}
	return 0, false
}

type loader struct {
	trustRoot string
	ownerOf   func(fs.FileInfo) (uint32, bool)
}

func (l loader) owner(fi fs.FileInfo) (uint32, bool) {
	if l.ownerOf != nil {
		return l.ownerOf(fi)
	}
	return fileOwner(fi)
}

func classifyOpenErr(path string, err error) error {
	switch {
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.EMLINK):
		return fmt.Errorf("%w: %s is a symbolic link: %w", ErrUntrusted, path, err)
	case errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("%w: %s is not a directory: %w", ErrUntrusted, path, err)
	case errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.EOPNOTSUPP):
		return fmt.Errorf("%w: %s is not a regular file: %w", ErrUntrusted, path, err)
	default:
		return err
	}
}

func (l loader) checkTrustInfo(path string, fi fs.FileInfo, wantDir bool) error {
	mode := fi.Mode()
	if wantDir {
		if !mode.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUntrusted, path)
		}
	} else if !mode.IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUntrusted, path)
	}
	special := fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if wantDir {
		special = fs.ModeSetuid | fs.ModeSticky
	}
	if mode&special != 0 {
		return fmt.Errorf("%w: %s has unsafe mode bits (%s)", ErrUntrusted, path, mode)
	}
	if uid, ok := l.owner(fi); !ok || uid != 0 {
		return fmt.Errorf("%w: %s is not owned by root", ErrUntrusted, path)
	}
	perm := mode.Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others (%04o)", ErrUntrusted, path, perm)
	}
	if !wantDir && perm&0o100 == 0 {
		return fmt.Errorf("%w: %s is not executable by owner (%04o)", ErrUntrusted, path, perm)
	}
	return nil
}

// openTrustedDir walks from trustRoot (default "/") to dir one path component
// at a time using openat(O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC), verifying
// every ancestor directory and dir itself so no unprivileged user can swap a
// directory or binary along the path.
func (l loader) openTrustedDir(dir string) (*os.File, string, error) {
	if !filepath.IsAbs(dir) {
		return nil, "", fmt.Errorf("%w: %s must be an absolute path", ErrUntrusted, dir)
	}
	for _, seg := range strings.Split(dir, string(os.PathSeparator)) {
		if seg == ".." {
			return nil, "", fmt.Errorf("%w: %s must not contain '..'", ErrUntrusted, dir)
		}
	}
	cleanDir := filepath.Clean(dir)
	root := l.trustRoot
	if root == "" {
		root = string(os.PathSeparator)
	}
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, cleanDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, "", fmt.Errorf("%w: %s is outside trust root %s", ErrUntrusted, cleanDir, root)
	}
	df, err := os.OpenFile(root, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", classifyOpenErr(root, err)
	}
	fi, err := df.Stat()
	if err == nil {
		err = l.checkTrustInfo(root, fi, true)
	}
	if err != nil {
		_ = df.Close()
		return nil, "", err
	}
	if rel == "." {
		return df, cleanDir, nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, part)
		childFD, err := unix.Openat(int(df.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		_ = df.Close()
		if err != nil {
			return nil, "", classifyOpenErr(cur, &os.PathError{Op: "open", Path: cur, Err: err})
		}
		df = os.NewFile(uintptr(childFD), cur)
		fi, err := df.Stat()
		if err == nil {
			err = l.checkTrustInfo(cur, fi, true)
		}
		if err != nil {
			_ = df.Close()
			return nil, "", err
		}
	}
	return df, cleanDir, nil
}

// Load starts every trusted module binary in dir (sorted by name). Because a
// module runs as the agent (root), dir must be an absolute path without ".."
// segments, every directory from "/" through dir must be a real directory
// owned by root without group/other write or setuid/sticky bits, and each
// executable must be a root-owned regular file with owner-execute permission,
// no group/other write, no setuid/setgid/sticky bits, and not a symbolic link.
// Each module process runs with working directory "/" and a minimal explicit
// environment (PATH=/usr/sbin:/usr/bin:/sbin:/bin); the agent's host
// environment is not inherited. A missing dir means no modules.
func Load(dir string, log *slog.Logger) ([]*Loaded, error) {
	return (loader{}).load(dir, log)
}

func (l loader) load(dir string, log *slog.Logger) ([]*Loaded, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if dir == "" {
		return nil, nil
	}
	df, cleanDir, err := l.openTrustedDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, ErrUntrusted) {
			log.Warn("refusing untrusted module directory", "dir", dir, "err", err)
		}
		return nil, err
	}
	defer df.Close()
	entries, err := df.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []*Loaded
	var errs []error
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(cleanDir, name)
		if e.Type()&(fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice) != 0 {
			err := fmt.Errorf("%w: %s is not a regular file", ErrUntrusted, path)
			log.Warn("skipping untrusted module", "path", path, "err", err)
			errs = append(errs, err)
			continue
		}
		fd, err := unix.Openat(int(df.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			err = classifyOpenErr(path, err)
			if errors.Is(err, ErrUntrusted) {
				log.Warn("skipping untrusted module", "path", path, "err", err)
				errs = append(errs, err)
			} else {
				log.Warn("skipping unreadable module", "path", path, "err", err)
				errs = append(errs, fmt.Errorf("module %s: %w", path, err))
			}
			continue
		}
		f := os.NewFile(uintptr(fd), path)
		info, statErr := f.Stat()
		_ = f.Close()
		if statErr != nil {
			errs = append(errs, fmt.Errorf("module %s: %w", path, statErr))
			continue
		}
		if info.Mode().IsDir() || (info.Mode().IsRegular() && info.Mode().Perm()&0o111 == 0) {
			continue
		}
		if err := l.checkTrustInfo(path, info, false); err != nil {
			log.Warn("skipping untrusted module", "path", path, "err", err)
			errs = append(errs, err)
			continue
		}
		mod, err := start(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("module %s: %w", path, err))
			continue
		}
		log.Info("module loaded", "name", mod.Info.Name, "version", mod.Info.Version, "path", path)
		out = append(out, mod)
	}
	return out, errors.Join(errs...)
}

const defaultModulePATH = "/usr/sbin:/usr/bin:/sbin:/bin"

func start(path string) (*Loaded, error) {
	cmd := exec.Command(path)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=" + defaultModulePATH}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          map[string]goplugin.Plugin{pluginName: &modulePlugin{}},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolNetRPC},
		// go-plugin would add the agent's whole environment (secrets included) to cmd.Env.
		SkipHostEnv: true,
		Logger:      hclog.New(&hclog.LoggerOptions{Output: io.Discard}),
	})
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, err
	}
	raw, err := rpcClient.Dispense(pluginName)
	if err != nil {
		client.Kill()
		return nil, err
	}
	remote := raw.(*Remote)
	info, err := remote.Info()
	if err != nil {
		client.Kill()
		return nil, err
	}
	m := ext.Module{Name: info.Name, Version: info.Version}
	if info.Authenticator {
		m.Authenticator = remote
		if info.PasswordChanger {
			m.Authenticator = passwordRemote{remote}
		}
	}
	if info.PrincipalResolver {
		m.PrincipalResolver = remote
	}
	if info.Authorizer {
		m.Authorizer = remote
	}
	if info.Sink {
		m.AuditSink = remote
	}
	if info.Router {
		m.Router, m.Ports = remote, info.Ports
	}
	m.Settings, m.Pages, m.RequiresLicense = info.Settings, info.Pages, info.RequiresLicense
	m.PublicPaths, m.LoginMethods = info.PublicPaths, info.LoginMethods
	if info.BranchPolicy {
		m.BranchPolicy = remoteBranchPolicy(remote, info)
	}
	if info.Configurable {
		m.Configurable = remote
	}
	if info.Handler {
		m.Handler = remote
	}
	if info.Starter {
		m.Starter = remote
	}
	if info.Masker {
		m.Masker = remote
	}
	if info.MaskPolicy {
		m.MaskPolicy = remote
	}
	if info.RefreshFollower {
		m.RefreshFollower = remote
	}
	if info.Storage {
		m.Storage, m.SharedStorage = storageRemote{remote}, info.SharedStorage
	}
	if info.ServerOptioner {
		m.ServerOptioner = remote
	}
	if info.RestoreFinisher {
		m.RestoreFinisher = remote
	}
	return &Loaded{Path: path, Info: info, Module: m, client: client}, nil
}

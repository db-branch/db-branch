package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/authfile"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/nodesys"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/infrastructure/sysmetrics"
	"github.com/db-branch/db-branch/internal/infrastructure/tlscert"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
	"github.com/db-branch/db-branch/internal/infrastructure/zfsarc"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/arccap"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/backup"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
	"github.com/db-branch/db-branch/web"
)

// ModulesDir is where module packages install their binaries (design.md §12).
const ModulesDir = "/usr/lib/db-branch/modules"

// AgentOptions tune RunAgent.
type AgentOptions struct {
	Version string
	Log     *slog.Logger
	// ExpireEvery is the TTL sweep interval (default 30s).
	ExpireEvery time.Duration
}

func authService(cfg config.Config) *auth.Service {
	return &auth.Service{Store: authfile.Store{Path: filepath.Join(cfg.DataDir, "auth.json")}, Hasher: authfile.Argon2{}, Clock: time.Now}
}

// AdvertiseHost is how clients reach this node.
func AdvertiseHost(cfg config.Config) string {
	if cfg.Advertise != "" {
		return cfg.Advertise
	}
	return OutboundIP()
}

// OutboundIP is the source address of the default route (no packet is sent),
// which avoids picking bridge interfaces such as docker0. Without a default
// route it falls back to the first non-loopback IPv4 address.
func OutboundIP() string {
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return "127.0.0.1"
}

// RunAgent serves the API on the UNIX socket and the console over HTTPS until
// ctx ends, recovering branches at start and expiring them periodically.
func RunAgent(ctx context.Context, cfg config.Config, opts AgentOptions) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	a, err := New(cfg)
	if err != nil {
		return err
	}
	if opts.Version != "" {
		version = opts.Version
	}
	pool := ""
	if svc, err := a.Branches(); err == nil && a.modStorage == nil {
		pool = svc.Pool // a ZFS pool: its size and use are reported
	}
	api := &lateHandler{}
	send := func(ctx context.Context, base domain.Name, from, to string, w io.Writer) error {
		svc, err := a.Branches()
		if err != nil {
			return err
		}
		return svc.SendBase(ctx, base, from, to, w)
	}
	artifacts := func(ctx context.Context, ref domain.RuntimeRef, arch string, part int) (io.ReadCloser, error) {
		path, err := a.Runtimes.ArtifactPath(ctx, ref, domain.PeerPlatform(arch), part)
		if err != nil {
			return nil, err
		}
		return os.Open(path)
	}
	a.Runtimes.InUse = func(ctx context.Context, ref domain.RuntimeRef) (bool, error) {
		svc, err := a.Branches()
		if err != nil {
			return false, nil
		}
		return svc.RuntimeInUse(ctx, ref)
	}
	cl, err := startCluster(ctx, cfg, pool, api, send, artifacts, log)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	bus := operation.NewBus(1000)
	node := hostname()
	ops := &operation.Manager{Bus: bus, Clock: time.Now, Node: node}
	srv := &httpapi.Server{
		Branches: func() (httpapi.Branches, error) { return a.Branches() },
		Runtimes: a.Runtimes,
		Ops:      ops,
		Bus:      bus,
		Auth:     authService(cfg),
		Host:     AdvertiseHost(cfg),
		Node:     node,
		Version:  opts.Version,
		Console:  web.Handler(),
	}
	system := nodesys.Default(execx.Runner{}.Run)
	srv.System = system.Get
	if cl != nil {
		defer cl.close()
		cl.system = system.Get
		node = cl.self.Node
		srv.Node, ops.Node = node, node
		// Every change, wherever it was made, reaches this node's subscribers
		// through the replicated store.
		ops.Store, ops.Bus = cl.store, nil
		go cl.publish(ctx, srv, bus)
		// One console password for the whole cluster.
		srv.Auth = &auth.Service{Store: raftstore.AdminStore{Store: cl.store}, Hasher: authfile.Argon2{}, Clock: time.Now}
		srv.OnRuntimesChanged = cl.beatNow
		cl.runtimes = func(ctx context.Context) []string {
			refs, _ := a.Runtimes.List(ctx)
			out := make([]string, 0, len(refs))
			for _, r := range refs {
				out = append(out, r.String())
			}
			return out
		}
		if svc, err := a.Branches(); err == nil {
			svc.Store, svc.Nodes, svc.DownAfter, svc.Node = cl.store, cl.store, NodeDownAfter, node
			if a.modStorage == nil {
				svc.Streams, svc.Source = zfs.New(execx.Runner{}), cl
			}
		}
		// Only the leader downloads runtimes from upstream; the others fetch
		// from it and verify against their own pinned digests.
		if cache, ok := a.Runtimes.Cache.(artifact.Cache); ok {
			cache.Fetch = cl.fetchArtifact
			a.Runtimes.Cache = cache
		}
		srv.Runtimes = a.Runtimes
		srv.Cluster = cl
		go cl.runtimeLoop(ctx, a.Runtimes, ops, log)
	}
	mods, err := plugin.Load(ModulesDir, log)
	if err != nil {
		log.Error("loading modules", "err", err)
	}
	var loaded []ext.Module
	var loadedNames []string
	moduleSettings := map[string][]ext.Setting{}
	for _, m := range mods {
		defer m.Close()
		loaded = append(loaded, m.Module)
		loadedNames = append(loadedNames, m.Info.Name)
		moduleSettings[m.Info.Name] = m.Info.Settings
		srv.Modules = append(srv.Modules, m.Info.Name+" "+m.Info.Version)
		if m.Module.RequiresLicense {
			srv.PaidModules = append(srv.PaidModules, m.Info.Name)
		}
	}
	srv.Ext, srv.Log = ext.Combine(loaded), log
	srv.ExtHandlers = map[string]ext.Handler{}
	for _, m := range mods {
		if m.Module.Handler != nil {
			srv.ExtHandlers[m.Info.Name] = m.Module.Handler
		}
		if len(m.Module.PublicPaths) > 0 {
			if srv.ExtPublic == nil {
				srv.ExtPublic = map[string][]string{}
			}
			srv.ExtPublic[m.Info.Name] = m.Module.PublicPaths
		}
		for _, lm := range m.Module.LoginMethods {
			srv.LoginMethods = append(srv.LoginMethods, httpapi.LoginMethod{Module: m.Info.Name, Label: lm.Label, Path: lm.Path, StatusPath: lm.StatusPath})
		}
		for _, p := range m.Module.Pages {
			srv.ExtPages = append(srv.ExtPages, httpapi.ModulePage{Module: m.Info.Name, Path: p.Path, Title: p.Title})
		}
	}
	attachStorage(ctx, a, mods, log)
	attachMaskers(a, mods, srv.Ext, maskLauncher{ops: ops, log: log, node: node, remote: srv.StartMaskOn, published: func(n domain.Name) {
		if cl == nil {
			srv.PublishBase(ctx, n)
		}
	}})
	if svc, err := a.Branches(); err == nil {
		wireModuleHooks(svc, srv.Ext)
	}
	startModules(ctx, a, cfg, node, mods, srv.BranchHost(), srv.ModuleAPI, log)
	if cl != nil {
		cl.modules = srv.Modules
		cl.checkpoints = svcCheckpoints(a)
		cl.beatNow()
	}
	problems := &moduleProblems{m: map[string]string{}}
	srv.ModuleProblems = problems.all
	arc := &arcNode{ctl: &arccap.Controller{Sys: zfsarc.System, Initial: func() (uint64, error) { return initialArc(cfg, zfsarc.System) }}}
	nodes := func() int { return 1 }
	if cl != nil {
		cl.problems = problems.all
		cl.arc = arc.report
		nodes = func() int { return len(cl.store.State().Nodes) }
	}
	if svc, err := a.Branches(); err == nil {
		var rolled func(context.Context) map[string]string
		if upd := wireUpdates(ctx, svc.Store, cl, node, bus, log); upd != nil {
			srv.Updates, rolled = upd, upd.Rolled
		}
		leading := func() bool { return true }
		if cl != nil {
			leading = cl.store.IsLeader
		}
		loadedRuntimes, runtimeWarnings, stopRuntimes := attachRuntimeModules(a, cfg, log)
		defer stopRuntimes()
		srv.RuntimeWarnings = runtimeWarnings
		mgr := wireModulesAndSettings(ctx, cfg, svc, srv, mods, moduleSettings, loadedNames, loadedRuntimes, nodes, problems, arc, rolled, leading, log)
		if cl != nil {
			cl.moduleChanged = mgr
		}
	}
	// Connection routing is a module (db-branch-proxysql): only with it does
	// the agent configure a proxy.
	if rt := proxyRouting(a, cl, node, srv.Host, srv.Ext.Router); rt != nil {
		srv.Routing = rt
		srv.ProxyPorts = map[domain.Engine]int{}
		for engine, port := range srv.Ext.Ports {
			srv.ProxyPorts[domain.Engine(engine)] = port
		}
		go routingLoop(ctx, rt, branchKicks(ctx, bus), 5*time.Second, log)
	}
	// Performance: a sample every 10s, the last hour kept here; the latest
	// travels in the heartbeat.
	sampler, history := &sysmetrics.Sampler{Root: "/"}, sysmetrics.NewHistory(360)
	history.Add(sampler.Sample())
	go every(ctx, 10*time.Second, func() { history.Add(sampler.Sample()) })
	srv.Metrics = history.List
	if pool != "" { // a node with its own ZFS pool (shared storage reports none)
		srv.Storage = func(ctx context.Context) (domain.StorageReport, error) {
			z := zfs.New(execx.Runner{})
			root := pool + "/db-branch"
			nodes, err := z.Tree(ctx, root)
			if err != nil {
				return domain.StorageReport{}, err
			}
			size, free, err := z.Space(ctx, pool)
			if err != nil {
				return domain.StorageReport{}, err
			}
			return domain.StorageReport{Pool: pool, PoolSize: size, PoolFree: free, Root: root, Nodes: nodes}, nil
		}
	}
	if cl != nil {
		cl.metrics = func() *domain.Metrics {
			if h := history.List(); len(h) > 0 {
				return &h[len(h)-1]
			}
			return nil
		}
	}
	srv.Providers = a.Providers()
	if svc, err := a.Branches(); err == nil {
		if st, ok := svc.Store.(domain.TokenStore); ok {
			tokens := &token.Service{Store: st, Clock: time.Now}
			srv.Tokens, srv.TokenAuth = tokens, tokens
		}
	}
	srv.Uploads = uploadStore(cfg)
	var backups *backup.Service
	if cl != nil {
		// Each node keeps its own backups of the (replicated) state, so a
		// copy survives the loss of any node.
		backups = &backup.Service{Source: cl.store, Node: node, Clock: time.Now,
			Dir:   backupDir{cfg: cfg},
			Keep:  func() int { return int(backupKeep.Load()) },
			Every: func() time.Duration { return time.Duration(backupEvery.Load()) }}
		srv.Backups = backups
	}
	srv.PoolFree = func() uint64 {
		if _, free := poolStats(ctx, pool); free > 0 {
			return free
		}
		return math.MaxUint64 // unknown: let the load report it
	}
	srv.OnPasswordReset = func() { _ = os.Remove(InitialPasswordFile) }
	api.set(srv.Handler(true))

	sock, err := listenSocket(cfg.Socket)
	if err != nil {
		return err
	}
	certFile, keyFile, err := tlscert.Ensure(filepath.Join(cfg.DataDir, "tls"), []string{node, srv.Host})
	if err != nil {
		sock.Close()
		return err
	}
	pair, _ := tls.LoadX509KeyPair(certFile, keyFile)
	srv.TLSCertPEM, _ = os.ReadFile(certFile)
	console, err := tls.Listen("tcp", cfg.Console.Listen, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		sock.Close()
		return fmt.Errorf("console listen %s: %w", cfg.Console.Listen, err)
	}

	local := &http.Server{Handler: srv.Handler(true), ReadHeaderTimeout: 10 * time.Second}
	remote := &http.Server{Handler: srv.Handler(false), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- local.Serve(sock) }()
	go func() { errs <- remote.Serve(console) }()
	log.Info("agent started", "version", opts.Version, "socket", cfg.Socket, "console", console.Addr().String())

	if cl != nil {
		cl.waitLeader(ctx)
		if err := cl.adoptPassword(authService(cfg).Store); err != nil {
			log.Error("sharing the console password with the cluster", "err", err)
		}
	}
	if err := ops.Recover(ctx); err != nil {
		log.Error("recovering operations", "err", err)
	}
	if svc, err := a.Branches(); err == nil {
		if err := svc.Adopt(ctx); err != nil {
			log.Error("adopting unowned bases and branches", "err", err)
		}
		if err := svc.Recover(ctx); err != nil {
			log.Error("recovery finished with errors", "err", err)
		}
		go expireLoop(ctx, log, opts.ExpireEvery, func() {
			expireOnce(ctx, log, a)
			refreshDue(ctx, log, svc, ops, func(n domain.Name) {
				if cl == nil {
					srv.PublishBase(ctx, n)
				}
			})
			if n := uploadStore(cfg).Sweep(UploadMaxAge); n > 0 {
				log.Info("unused uploads deleted", "count", n)
			}
			if backups != nil && backups.Due() {
				if f, err := backups.Take(ctx); err != nil {
					log.Error("backing up the cluster state", "err", err)
				} else {
					log.Info("cluster state backed up", "file", f.Name)
				}
			}
		})
		if cl != nil {
			go cl.replicationLoop(ctx, svc.Replicate, log)
			go cl.healthLoop(ctx, svc, log)
			go cl.notifyLoop(ctx, newNotifier(cl.store), log)
			go cl.membershipLoop(ctx, log)
		}
	}

	var result error
	select {
	case <-ctx.Done():
	case err := <-errs:
		result = err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = local.Shutdown(shutdown)
	_ = remote.Shutdown(shutdown)
	log.Info("agent stopped")
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}

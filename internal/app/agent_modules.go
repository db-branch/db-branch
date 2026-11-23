package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/apt"
	"github.com/db-branch/db-branch/internal/infrastructure/backupdir"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/licenseclient"
	"github.com/db-branch/db-branch/internal/infrastructure/modmask"
	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/infrastructure/portreserve"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/arccap"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/internal/usecase/module"
	"github.com/db-branch/db-branch/internal/usecase/moduledata"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

// coreSettings are db-branch's own cluster-wide settings (Settings page).
func coreSettings(cfg config.Config) []ext.Setting {
	return []ext.Setting{
		{Key: "branch.memory_mb", Label: "Branch memory (MB)", Type: "int", Default: strconv.Itoa(cfg.Branch.MemoryMB),
			Help: "InnoDB buffer pool / shared_buffers of each branch server. Branches started from now on use it."},
		{Key: "branch.max_checkpoints", Label: "Checkpoints per branch", Type: "int", Default: strconv.Itoa(domain.DefaultMaxCheckpoints),
			Help: "The most checkpoints you take yourself that one branch may hold, over all its lines of history. The checkpoints taken automatically before a restore are not counted and never block a restore: the newest five are kept, older ones are deleted. Each checkpoint keeps the data it protects, so it uses disk space as the branch changes. 0: none."},
		{Key: "branch.default_ttl", Label: "Default TTL", Type: "duration", Default: "",
			Help: "Expiry for branches created without one, e.g. 72h. Empty: no expiry."},
		{Key: "branch.refquota_gb", Label: "Largest branch size (GB)", Type: "int", Default: "0",
			Help: "Most data one branch may reference (ZFS refquota), set on branches created from now on. A branch that reaches it gets write errors; the others and the pool are unaffected. 0: no limit."},
		{Key: "branch.max_per_user", Label: "Branches per user", Type: "int", Default: "0",
			Help: "The most branches one signed-in user may have at once. 0: no limit."},
		{Key: "branch.max_ttl", Label: "Longest branch TTL", Type: "duration", Default: "",
			Help: "No branch may live longer than this from creation or extension; a branch created without a TTL gets it. e.g. 168h. Empty: no limit."},
		{Key: "base.refresh_max_gb", Label: "Largest refresh dump (GB)", Type: "int", Default: "0",
			Help: "A scheduled refresh whose source dump grows past this is stopped and the dump deleted. A refresh is also stopped when the node's disk would drop below 1 GiB free. 0: only the disk check."},
		{Key: "ports.min", Label: "First branch port", Type: "int", Default: strconv.Itoa(cfg.Ports.Min)},
		{Key: "ports.max", Label: "Last branch port", Type: "int", Default: strconv.Itoa(cfg.Ports.Max)},
		{Key: "console.session_ttl", Label: "Console session", Type: "duration", Default: auth.DefaultSessionTTL.String(),
			Help: "How long a console login lasts without use."},
		{Key: "license.key", Label: "License key", Type: "secret", Default: "",
			Help: "Unlocks the paid modules below. Every node gets it."},
		{Key: licensing.AutoRenewSetting, Label: "Renew the licence automatically", Type: "bool", Default: "true",
			Help: "A licence bought as a monthly or yearly subscription is renewed by the cluster itself: shortly before it runs out, the leader asks the licence service over HTTPS for the new key, sending only the current key, and saves it. Turn this off on air-gapped clusters and paste the new key by hand (Modules, License); then nothing contacts the network."},
		{Key: "branch.open_password", Label: "Open branch password", Type: "secret", Default: "dbbranch",
			Help: "Shared password of branches anyone may connect to. Letters and digits only."},
		{Key: "branch.anonymous_ttl", Label: "Anonymous branch TTL", Type: "duration", Default: "24h",
			Help: "Longest life of a branch created without logging in (from a public snapshot)."},
		{Key: "branch.anonymous_max", Label: "Anonymous branches", Type: "int", Default: "10",
			Help: "How many branches created without logging in may exist at once, over all bases. A base can lower this for itself (Bases page, Without sign-in). 0: none."},
		{Key: "branch.anonymous_rate", Label: "Anonymous creations per IP per hour", Type: "int", Default: "5",
			Help: "How many branches one client address may create per hour without logging in. Counted on each node in memory, never stored. Behind a reverse proxy every client shares the proxy's address. 0: no limit."},
		{Key: "upload.max_mb", Label: "Largest dump upload (MB)", Type: "int", Default: "20480",
			Help: "Dumps sent from the console (or POST /v1/uploads) larger than this are refused. They are also refused when the node lacks the disk space."},
		{Key: "zfs.arc_max", Label: "ZFS ARC max", Type: "string", Default: "",
			Help: "Most memory ZFS may use to cache reads on each node: a percentage of its memory (50%) or a size (8GB). Raising it can speed up branch creation for large bases but leaves less memory for branch servers. Empty: the value chosen at db-branch init."},
		{Key: "backup.interval", Label: "Backup interval", Type: "duration", Default: "6h",
			Help: "Every node saves the cluster state (settings, users, tokens, module data) this often. 0: never."},
		{Key: "backup.keep", Label: "Backups kept", Type: "int", Default: "14", Help: "Newest backups each node keeps."},
		{Key: "backup.dir", Label: "Backup directory", Type: "string", Default: "",
			Help: "Where each node writes them (readable by root only). Empty: /var/lib/db-branch/backups. Mount shared or off-site storage here."},
		{Key: "notify.slack_url", Label: "Slack webhook URL", Type: "secret", Default: "",
			Help: "An incoming webhook (https://hooks.slack.com/...). Gets branches about to expire, failed operations and nodes going down or coming back. Empty: off."},
		{Key: "notify.webhook_url", Label: "Webhook URL", Type: "string", Default: "",
			Help: "Gets the same notices as JSON (event, level, subject, text, time, owner). Empty: off."},
		{Key: "notify.webhook_secret", Label: "Webhook secret", Type: "secret", Default: "",
			Help: "Signs each webhook body: X-Db-Branch-Signature: sha256=<HMAC-SHA256 hex>."},
		{Key: "notify.expiry_warning", Label: "Warn before expiry", Type: "duration", Default: "1h",
			Help: "How long before a branch expires to warn. 0: no expiry warnings."},
		{Key: "audit.fail_closed", Label: "Refuse changes that cannot be audited", Type: "bool", Default: "false",
			Help: "Off (default): if the audit sink fails, the change goes ahead and the failure is only logged. On: every change is first recorded in the audit log and refused with 503 audit_unavailable when that fails. Reads that are not audited are unaffected."},
		{Key: "update.auto_check", Label: "Check for updates daily", Type: "bool", Default: "false",
			Help: "The leader asks the package repository once a day for newer db-branch versions. Off: the repository is contacted only when an admin clicks Check for updates."},
		{Key: "base.prune_snapshots", Label: "Prune old snapshots", Type: "bool", Default: "true",
			Help: "Delete a base's older snapshots once a newer one exists and no branch uses them."},
	}
}

// wireModulesAndSettings sets up cluster-wide modules and settings and keeps
// this node in line with them. It returns a function to call when the
// recorded modules change.
func wireModulesAndSettings(ctx context.Context, cfg config.Config, svc *dbbranch.Service, srv *httpapi.Server,
	mods []*plugin.Loaded, moduleSettings map[string][]ext.Setting, loadedNames, loadedRuntimes []string, nodes func() int,
	problems *moduleProblems, arc *arcNode, rolled func(context.Context) map[string]string, rolling func(context.Context) bool, leading func() bool, log *slog.Logger) func() {
	checkNotifyURL := func(v string) error {
		return notifier.ValidateURL(v, cfg.Notify.AllowPrivateIPs)
	}
	set := &settings.Service{Core: coreSettings(cfg), Modules: func() map[string][]ext.Setting { return moduleSettings },
		Checks: map[string]func(string) error{
			"zfs.arc_max":            arc.ctl.Check,
			"branch.max_checkpoints": settings.NonNegativeInt,
			"branch.refquota_gb":     settings.NonNegativeInt,
			"base.refresh_max_gb":    settings.NonNegativeInt,
			"branch.max_per_user":    settings.NonNegativeInt,
			"backup.dir":             backupdir.ValidatePath,
			"notify.slack_url":       checkNotifyURL,
			"notify.webhook_url":     checkNotifyURL,
		}}
	if st, ok := svc.Store.(domain.SettingsStore); ok {
		set.Store = st
		srv.Settings = set
		srv.Preferences = st
	}
	var defaultTTL, sessionTTL, anonTTL, anonMax, anonRate atomic.Int64
	var openPassword atomic.Value
	openPassword.Store("dbbranch")
	anonTTL.Store(int64(24 * time.Hour))
	anonMax.Store(10)
	anonRate.Store(5)
	svc.OpenPassword = func() string { return openPassword.Load().(string) }
	srv.Anonymous = func() (time.Duration, int) { return time.Duration(anonTTL.Load()), int(anonMax.Load()) }
	srv.AnonymousRate = func() int { return int(anonRate.Load()) }
	var auditFailClosed atomic.Bool
	srv.AuditFailClosed = auditFailClosed.Load
	srv.DefaultTTL = func() time.Duration { return time.Duration(defaultTTL.Load()) }
	srv.Auth.TTL = func() time.Duration { return time.Duration(sessionTTL.Load()) }
	// The licence service exists before the modules are managed: a loaded
	// paid module begins the cluster's trial.
	lic := &licensing.Service{Root: licenseRoot(), Clock: time.Now, Nodes: nodes, Settings: set}
	lic.Trial, _ = svc.Store.(domain.TrialStore)
	var applyMu sync.Mutex
	applied := map[string]string{}
	apply := func() {
		applyMu.Lock()
		defer applyMu.Unlock()
		if set.Store == nil {
			return
		}
		if v, err := set.Values(ctx, settings.CoreScope); err == nil {
			mem, _ := strconv.Atoi(v["branch.memory_mb"])
			lo, _ := strconv.Atoi(v["ports.min"])
			hi, _ := strconv.Atoi(v["ports.max"])
			if mem > 0 && lo > 0 && hi >= lo {
				svc.SetLimits(mem, dbbranch.PortRange{Min: lo, Max: hi})
			}
			if lo > 0 && hi >= lo && applied["core:ports"] != v["ports.min"]+"-"+v["ports.max"] {
				applied["core:ports"] = v["ports.min"] + "-" + v["ports.max"]
				// Keep outgoing connections off the branch ports, or a server
				// started on a port one of them holds fails ("Address already in use").
				if changed, err := portreserve.Reserve(lo, hi); err != nil {
					log.Warn("reserving the branch ports", "min", lo, "max", hi, "err", err)
				} else if changed {
					log.Info("reserved the branch ports (net.ipv4.ip_local_reserved_ports)", "min", lo, "max", hi)
				}
			}
			d, _ := time.ParseDuration(v["branch.default_ttl"])
			defaultTTL.Store(int64(d))
			d, _ = time.ParseDuration(v["console.session_ttl"])
			sessionTTL.Store(int64(d))
			if pw := v["branch.open_password"]; validOpenPassword(pw) {
				openPassword.Store(pw)
			}
			if d, err := time.ParseDuration(v["branch.anonymous_ttl"]); err == nil && d > 0 {
				anonTTL.Store(int64(d))
			}
			if n, err := strconv.Atoi(v["branch.anonymous_max"]); err == nil && n >= 0 {
				anonMax.Store(int64(n))
			}
			if n, err := strconv.Atoi(v["branch.anonymous_rate"]); err == nil && n >= 0 {
				anonRate.Store(int64(n))
			}
			if n, err := strconv.Atoi(v["branch.max_checkpoints"]); err == nil && n >= 0 {
				maxCheckpoints.Store(int64(n))
			}
			if n, err := strconv.Atoi(v["branch.refquota_gb"]); err == nil && n >= 0 {
				refQuotaGB.Store(int64(n))
			}
			if n, err := strconv.Atoi(v["branch.max_per_user"]); err == nil && n >= 0 {
				maxPerUser.Store(int64(n))
			}
			if n, err := strconv.Atoi(v["base.refresh_max_gb"]); err == nil && n >= 0 {
				refreshMaxGB.Store(int64(n))
			}
			d, _ = time.ParseDuration(v["branch.max_ttl"])
			maxBranchTTL.Store(int64(max(d, 0)))
			pruneSnapshots.Store(v["base.prune_snapshots"] != "false")
			updateAutoCheck.Store(v["update.auto_check"] == "true")
			closed, _ := strconv.ParseBool(v["audit.fail_closed"])
			auditFailClosed.Store(closed)
			if d, err := time.ParseDuration(v["backup.interval"]); err == nil && d >= 0 {
				backupEvery.Store(int64(d))
			}
			if n, err := strconv.Atoi(v["backup.keep"]); err == nil && n > 0 {
				backupKeep.Store(int64(n))
			}
			if dir := v["backup.dir"]; dir == "" {
				backupPath.Store("")
				delete(applied, "core:backup.dir.err")
			} else if err := backupdir.ValidatePath(dir); err != nil {
				backupPath.Store("")
				if applied["core:backup.dir.err"] != dir {
					applied["core:backup.dir.err"] = dir
					log.Warn("ignoring invalid backup.dir setting", "dir", dir, "err", err)
				}
			} else {
				backupPath.Store(dir)
				delete(applied, "core:backup.dir.err")
			}
			notifySlack.Store(v["notify.slack_url"])
			notifyWebhook.Store(v["notify.webhook_url"])
			notifySecret.Store(v["notify.webhook_secret"])
			if d, err := time.ParseDuration(v["notify.expiry_warning"]); err == nil && d >= 0 {
				notifyWarn.Store(int64(d))
			}
			if mb, err := strconv.ParseInt(v["upload.max_mb"], 10, 64); err == nil && mb > 0 {
				uploadMaxBytes.Store(mb << 20)
			}
			arc.sync(v["zfs.arc_max"], log)
		}
		core, _ := set.Values(ctx, settings.CoreScope)
		trial := ""
		if len(srv.PaidModules) > 0 {
			if at, err := lic.StartTrial(ctx); err == nil && !at.IsZero() {
				trial = at.Format(time.RFC3339)
			}
		}
		for _, m := range mods {
			if m.Module.Configurable == nil {
				continue
			}
			v, err := set.Values(ctx, m.Info.Name)
			if err != nil {
				v = map[string]string{} // a module without settings still gets the licence
			}
			v[ext.LicenseKey], v[ext.NodesKey], v[ext.TrialKey] = core["license.key"], strconv.Itoa(nodes()), trial
			b, _ := json.Marshal(v)
			if applied[m.Info.Name] == string(b) {
				continue
			}
			if err := m.Module.Configurable.Configure(ctx, v); err != nil {
				if problems.set(m.Info.Name, err.Error()) {
					log.Warn("configuring module", "module", m.Info.Name, "err", err)
				}
				continue
			}
			if problems.set(m.Info.Name, "") {
				log.Info("module configured", "module", m.Info.Name)
			}
			applied[m.Info.Name] = string(b)
		}
	}
	apply()
	go every(ctx, 5*time.Second, apply)

	store, ok := svc.Store.(domain.ModuleStore)
	if !ok {
		return func() {}
	}
	mgr := &module.Service{Store: store, Packages: apt.Packages{Run: execx.Runner{}}, Rolled: rolled, Busy: rolling}
	if ns, ok := svc.Store.(domain.NodeStore); ok {
		mgr.Nodes = ns.ListNodes // runtime modules wait until every node knows them
	}
	srv.ModuleAdmin = mgr
	notices := newNotifier(nil, cfg.Notify.AllowPrivateIPs, 0)
	notifyFn := func(ctx context.Context, event, level, subject, text string) {
		if err := notices.Publish(ctx, event, level, subject, text); err != nil {
			log.Warn("sending a notice", "event", event, "err", err)
		}
	}
	if srv.Settings != nil {
		lic.Modules = mgr
		srv.Licensing = lic
		// Renewing a subscription's key is the leader's job.
		renewer := &licensing.Renewer{Service: lic, Client: &licenseclient.Client{Version: version}, Leader: leading, Log: log, Notify: notifyFn}
		// The cluster's anonymous id travels with the renewal request.
		if ids, ok := svc.Store.(domain.ClusterIDStore); ok {
			renewer.Cluster = ids
		}
		srv.Renewal = renewer
		go renewer.Run(ctx)
	}
	srv.PendingModules = func() []string {
		specs, err := store.ListModules(context.Background())
		if err != nil {
			return nil
		}
		pending := domain.PendingModules(specs, loadedNames)
		for _, engine := range domain.PendingRuntimeModules(specs, loadedRuntimes) {
			pending = append(pending, domain.RuntimeModuleName(engine))
		}
		return pending
	}
	kick := make(chan struct{}, 1)
	syncer := &moduleSyncer{
		adopt:         mgr.Adopt,
		sync:          mgr.SyncReport,
		problems:      problems,
		node:          srv.Node,
		bus:           srv.Bus,
		notify:        notifyFn,
		log:           log,
		baseDelay:     moduleSyncInterval,
		maxBackoff:    moduleSyncMaxBackoff,
		kickDelay:     moduleSyncKickDelay,
		failThreshold: moduleSyncFailThreshold,
	}
	go syncer.run(ctx, kick)
	return func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

const (
	moduleSyncInterval      = 30 * time.Second
	moduleSyncKickDelay     = 2 * time.Second
	moduleSyncMaxBackoff    = 30 * time.Minute
	moduleSyncFailThreshold = 2
)

type moduleSyncer struct {
	adopt         func(context.Context) error
	sync          func(context.Context) (module.SyncResult, error)
	problems      *moduleProblems
	node          string
	bus           *operation.Bus
	notify        func(ctx context.Context, event, level, subject, text string)
	log           *slog.Logger
	baseDelay     time.Duration
	maxBackoff    time.Duration
	kickDelay     time.Duration
	failThreshold int
	after         func(time.Duration) <-chan time.Time

	adopted         bool
	backoffFailures int
	moduleFailures  map[string]int
	aheadWarned     map[string]string
	failReported    map[string]string
}

func (r *moduleSyncer) afterTimer(d time.Duration) <-chan time.Time {
	if r.after != nil {
		return r.after(d)
	}
	return time.After(d)
}

func (r *moduleSyncer) backoff(failures int) time.Duration {
	if failures <= 0 {
		return r.baseDelay
	}
	d := r.baseDelay
	for range failures {
		if d >= r.maxBackoff || d > r.maxBackoff/2 {
			return r.maxBackoff
		}
		d *= 2
	}
	return min(d, r.maxBackoff)
}

func (r *moduleSyncer) run(ctx context.Context, kick <-chan struct{}) {
	for {
		wait := r.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.afterTimer(wait):
		case <-kick:
			// Reset backoff wait when the cluster's module record changes,
			// without clearing ongoing per-module failure counts/problems.
			r.backoffFailures = 0
			// Let the request that changed the record answer first: a
			// package change restarts this agent.
			select {
			case <-ctx.Done():
				return
			case <-r.afterTimer(r.kickDelay):
			}
		}
	}
}

func (r *moduleSyncer) step(ctx context.Context) time.Duration {
	// Retried: right after a start the cluster may not accept the
	// record yet (protocol not known to be high enough).
	if !r.adopted {
		if err := r.adopt(ctx); err != nil {
			r.log.Debug("recording installed modules", "err", err)
		} else {
			r.adopted = true
		}
	}
	res, err := r.sync(ctx)
	if ctx.Err() != nil || res.Skipped {
		return r.baseDelay
	}
	keepProblems := map[string]bool{}
	seenAhead := map[string]bool{}
	for _, a := range res.Ahead {
		seenAhead[a.Name] = true
		keepProblems[a.Name] = true
		msg := fmt.Sprintf("installed %s is newer than cluster record %s; skipping downgrade (run `db-branch module install %s --version %s` once all nodes are upgraded)", a.Installed, a.Recorded, a.Name, a.Installed)
		r.problems.setSync(a.Name, msg)
		sig := a.Installed + "->" + a.Recorded
		if r.aheadWarned == nil {
			r.aheadWarned = map[string]string{}
		}
		if r.aheadWarned[a.Name] != sig {
			r.aheadWarned[a.Name] = sig
			r.log.Warn("installed module is newer than cluster record; skipping downgrade", "module", a.Name, "installed", a.Installed, "recorded", a.Recorded)
			if r.bus != nil {
				r.bus.PublishTo(operation.Audience{Action: "module.read"}, "module.version_ahead", map[string]string{
					"node": r.node, "module": a.Name, "package": a.Package, "installed": a.Installed, "recorded": a.Recorded,
				})
			}
			if r.notify != nil {
				r.notify(ctx, "module.version_ahead", "warning", a.Name,
					fmt.Sprintf("Node %s runs %s %s, newer than cluster record %s; skipping downgrade. Run `db-branch module install %s --version %s` once all nodes are upgraded.", r.node, a.Package, a.Installed, a.Recorded, a.Name, a.Installed))
			}
		}
	}
	for k := range r.aheadWarned {
		if !seenAhead[k] {
			delete(r.aheadWarned, k)
		}
	}

	if err == nil {
		if r.backoffFailures > 0 || len(r.moduleFailures) > 0 {
			r.log.Info("module sync recovered", "after_failures", r.backoffFailures)
		}
		r.backoffFailures = 0
		r.moduleFailures = nil
		r.failReported = nil
		r.problems.clearSyncExcept(keepProblems)
		if res.Changed {
			r.log.Info("modules changed; the agent restarts to load them")
		}
		return r.baseDelay
	}

	r.backoffFailures++
	wait := r.backoff(r.backoffFailures)
	r.log.Warn("installing modules", "err", err, "failures", r.backoffFailures, "retry_in", wait)

	failed := res.Failed
	if len(failed) == 0 {
		failed = []module.FailedModule{{Name: "_sync", Op: "sync", Err: err}}
	}
	if r.moduleFailures == nil {
		r.moduleFailures = map[string]int{}
	}
	if r.failReported == nil {
		r.failReported = map[string]string{}
	}
	seenFailed := map[string]bool{}
	for _, f := range failed {
		seenFailed[f.Name] = true
		r.moduleFailures[f.Name]++
		count := r.moduleFailures[f.Name]
		if count < r.failThreshold {
			continue
		}
		keepProblems[f.Name] = true
		msg := fmt.Sprintf("module %s failed after %d attempts: %v", f.Op, count, f.Err)
		r.problems.setSync(f.Name, msg)
		if r.failReported[f.Name] != f.Op {
			r.failReported[f.Name] = f.Op
			errText := f.Err.Error()
			if r.bus != nil {
				r.bus.PublishTo(operation.Audience{Action: "module.read"}, "module.sync_failed", map[string]any{
					"node": r.node, "module": f.Name, "op": f.Op, "error": errText, "failures": count,
				})
			}
			if r.notify != nil {
				r.notify(ctx, "module.sync_failed", "error", f.Name,
					fmt.Sprintf("Module %s (%s) sync failed on %s after %d attempts: %s", f.Name, f.Op, r.node, count, errText))
			}
		}
	}
	for k := range r.moduleFailures {
		if !seenFailed[k] {
			delete(r.moduleFailures, k)
		}
	}
	for k := range r.failReported {
		if !seenFailed[k] {
			delete(r.failReported, k)
		}
	}
	r.problems.clearSyncExcept(keepProblems)
	return wait
}

// moduleKicks calls kick whenever a module.updated event arrives on bus (used
// in single-node mode where Raft does not drive cl.moduleChanged).
func moduleKicks(ctx context.Context, bus *operation.Bus, kick func()) {
	sub := bus.Subscribe(0)
	go func() {
		for {
			for open := true; open; {
				select {
				case <-ctx.Done():
					sub.Close()
					return
				case ev, ok := <-sub.Events():
					if !ok {
						open = false
					} else if ev.Type == "module.updated" {
						kick()
					}
				}
			}
			kick()
			if ctx.Err() != nil {
				return
			}
			sub = bus.Subscribe(0)
		}
	}()
}

// arcNode applies the ARC cap setting on this node and remembers why it
// could not, for the heartbeat.
type arcNode struct {
	ctl     *arccap.Controller
	problem atomic.Value // string
}

func (a *arcNode) sync(raw string, log *slog.Logger) {
	msg := ""
	if err := a.ctl.Sync(raw); err != nil {
		msg = err.Error()
	}
	if old, _ := a.problem.Load().(string); old != msg {
		a.problem.Store(msg)
		if msg != "" {
			log.Warn("applying the ZFS ARC cap", "problem", msg)
		} else {
			log.Info("ZFS ARC cap applied")
		}
	}
}

// report is the cap in effect (MB) and the problem applying the setting.
func (a *arcNode) report() (uint64, string) {
	p, _ := a.problem.Load().(string)
	return a.ctl.EffectiveMB(), p
}

// moduleProblems are the last Configure or module-sync errors on this node
// (e.g. "licence expired" or sync failure), shown in the console and metrics.
type moduleProblems struct {
	mu       sync.Mutex
	m        map[string]string
	sync     map[string]string
	onChange func()
}

func (p *moduleProblems) notifyChange() {
	if p.onChange != nil {
		p.onChange()
	}
}

// set records a module's configure problem ("" when none) and reports whether
// it changed, so it is logged once rather than at every retry.
func (p *moduleProblems) set(name, problem string) bool {
	p.mu.Lock()
	if p.m == nil {
		p.m = map[string]string{}
	}
	changed := p.m[name] != problem
	if problem == "" {
		delete(p.m, name)
	} else {
		p.m[name] = problem
	}
	p.mu.Unlock()
	if changed {
		p.notifyChange()
	}
	return changed
}

// setSync records a module's sync or version-mismatch problem ("" when none).
func (p *moduleProblems) setSync(name, problem string) bool {
	p.mu.Lock()
	if p.sync == nil {
		p.sync = map[string]string{}
	}
	changed := p.sync[name] != problem
	if problem == "" {
		delete(p.sync, name)
	} else {
		p.sync[name] = problem
	}
	p.mu.Unlock()
	if changed {
		p.notifyChange()
	}
	return changed
}

// clearSyncExcept removes sync problems for modules not in keep.
func (p *moduleProblems) clearSyncExcept(keep map[string]bool) bool {
	p.mu.Lock()
	changed := false
	for k := range p.sync {
		if !keep[k] {
			delete(p.sync, k)
			changed = true
		}
	}
	p.mu.Unlock()
	if changed {
		p.notifyChange()
	}
	return changed
}

func (p *moduleProblems) all() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for k, v := range p.m {
		out[k] = v
	}
	for k, v := range p.sync {
		if prev, ok := out[k]; ok && prev != "" {
			out[k] = prev + "; " + v
		} else {
			out[k] = v
		}
	}
	return out
}

// wireModuleHooks lets modules configure branch servers (recording) and
// work on a branch right after a restore they started.
func wireModuleHooks(svc *dbbranch.Service, m ext.Module) {
	if o := m.ServerOptioner; o != nil {
		svc.ServerOptions = func(ctx context.Context, br domain.Branch, maintenance bool) ([]string, error) {
			opts, err := o.ServerOptions(ctx, httpapi.ServerBranchOf(br, maintenance))
			return opts.Config, err
		}
		svc.RecordNew = func(ctx context.Context, br domain.Branch) bool {
			b := httpapi.ServerBranchOf(br, false)
			b.Creating = true
			opts, err := o.ServerOptions(ctx, b)
			return err == nil && opts.Record
		}
	}
	if f := m.RestoreFinisher; f != nil {
		svc.AfterRestore = func(ctx context.Context, br domain.Branch, note string) error {
			return f.AfterRestore(ctx, httpapi.ServerBranchOf(br, true), note)
		}
	}
}

// startModules gives each module its host: node-local directory and its
// cluster-wide store (unavailable before `db-branch init`).
func startModules(ctx context.Context, a *App, cfg config.Config, node string, mods []*plugin.Loaded, branches ext.BranchHost, api func(module string) ext.ModuleAPI, log *slog.Logger) {
	var data domain.ModuleDataStore
	if svc, err := a.Branches(); err == nil {
		data, _ = svc.Store.(domain.ModuleDataStore)
	}
	for _, m := range mods {
		if m.Module.Starter == nil {
			continue
		}
		dir := filepath.Join(cfg.DataDir, "modules", m.Info.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Error("module directory", "module", m.Info.Name, "err", err)
			continue
		}
		h := ext.Host{Node: node, DataDir: dir, Store: moduledata.For(data, m.Info.Name), Branches: branches, API: api(m.Info.Name)}
		if err := m.Module.Starter.Start(ctx, h); err != nil {
			log.Error("starting module", "module", m.Info.Name, "err", err)
		}
	}
}

// attachMaskers makes the masker modules available by name, governed by
// the (first) module with a mask policy.
func attachMaskers(a *App, mods []*plugin.Loaded, combined ext.Module, launcher maskLauncher) {
	svc, err := a.Branches()
	if err != nil {
		return
	}
	svc.Maskers = map[string]domain.Masker{}
	for _, m := range mods {
		if m.Module.Masker != nil {
			svc.Maskers[m.Info.Name] = modmask.Masker{M: m.Module.Masker}
		}
	}
	if combined.MaskPolicy != nil {
		svc.MaskPolicy = modmask.Policy{P: combined.MaskPolicy}
	}
	if combined.RefreshFollower != nil {
		launcher.svc = svc
		svc.Follower, svc.Launcher = modmask.Follower{F: combined.RefreshFollower}, launcher
		svc.Logf = func(format string, args ...any) { launcher.log.Warn(fmt.Sprintf(format, args...)) }
	}
}

// maskLauncher starts the mask runs that follow a refresh as base.mask
// operations, like the ones the API starts.
type maskLauncher struct {
	svc       *dbbranch.Service
	ops       *operation.Manager
	log       *slog.Logger
	node      string
	remote    func(ctx context.Context, node string, req dbbranch.MaskRequest) error
	published func(domain.Name)
}

func (l maskLauncher) Busy(target domain.Name) bool {
	_, busy := l.ops.Active("base", target.String())
	return busy
}

func (l maskLauncher) Start(node string, req dbbranch.MaskRequest) error {
	if node != "" && node != l.node {
		return l.remote(context.Background(), node, req) // the target's node runs it
	}
	_, err := l.ops.Start("base.mask", operation.Target{Resource: "base", Name: req.Target.String()},
		func(ctx context.Context, report func(string)) error {
			_, err := l.svc.MaskBase(ctx, req, report)
			if err != nil {
				l.log.Error("mask after a refresh failed", "target", req.Target.String(), "source", req.Source.String(), "err", err)
			}
			l.published(req.Target)
			return err
		})
	return err
}

// attachStorage gives a node set up on a module's storage (db-branch init
// --storage) that module's storage, once it is loaded.
func attachStorage(ctx context.Context, a *App, mods []*plugin.Loaded, log *slog.Logger) {
	if a.modStorage == nil {
		return
	}
	svc, err := a.Branches()
	if err != nil {
		return
	}
	for _, m := range mods {
		if m.Info.Name != a.modStorage.Module || m.Module.Storage == nil {
			continue
		}
		a.modStorage.Attach(m.Module.Storage)
		if m.Module.SharedStorage {
			svc.Shared = a.modStorage
		}
		log.Info("using module storage", "module", m.Info.Name, "shared", m.Module.SharedStorage)
		return
	}
	log.Warn("storage module not loaded: bases and branches are unavailable until it is installed", "module", a.modStorage.Module)
}

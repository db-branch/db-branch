package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/arccap"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/module"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

func TestWireModulesAndSettings_backupDirの危険なパスを拒否する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st := &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")}
	// Pre-populate an invalid backup.dir as if upgraded from an older version.
	if err := st.PutSettings(ctx, settings.CoreScope, map[string]string{"backup.dir": "/etc"}); err != nil {
		t.Fatal(err)
	}

	svc := &dbbranch.Service{Store: st}
	srv := &httpapi.Server{Auth: &auth.Service{}}
	arc := &arcNode{ctl: &arccap.Controller{}}
	problems := &moduleProblems{m: map[string]string{}}

	backupPath.Store("")
	t.Cleanup(func() { backupPath.Store("") })

	wire := func() {
		wCtx, wCancel := context.WithCancel(ctx)
		_ = wireModulesAndSettings(wCtx, cfg, svc, srv, nil, nil, nil, nil,
			func() int { return 1 }, problems, arc, nil, nil, func() bool { return true }, discardLog())
		wCancel()
	}

	wire()

	// Pre-existing invalid setting must not be applied to backupPath.
	if got, _ := backupPath.Load().(string); got != "" {
		t.Errorf("backupPath = %q after invalid stored setting, want empty", got)
	}

	// Putting a dangerous path through settings service must be rejected.
	for _, bad := range []string{"/", "/etc", "/var/lib", "/var/run/db-branch", "/root/.ssh", "relative/path", "/var/backups/../etc"} {
		if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": bad}); !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("Put(backup.dir=%q) = %v, want ErrInvalid", bad, err)
		}
	}

	// Putting a valid path succeeds and is picked up by wireModulesAndSettings.
	valid := filepath.Join(t.TempDir(), "backups")
	if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": valid}); err != nil {
		t.Fatalf("Put valid backup.dir: %v", err)
	}
	wire()
	if got, _ := backupPath.Load().(string); got != valid {
		t.Errorf("backupPath = %q, want %q", got, valid)
	}

	// Clearing resets back to default empty.
	if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": ""}); err != nil {
		t.Fatalf("Put empty backup.dir: %v", err)
	}
	wire()
	if got, _ := backupPath.Load().(string); got != "" {
		t.Errorf("backupPath after clear = %q, want empty", got)
	}
}

func TestResourceLimits_設定値がサービスの上限に変換され既定は無制限(t *testing.T) {
	refQuotaGB.Store(0)
	maxPerUser.Store(0)
	maxBranchTTL.Store(0)
	if got := resourceLimits(); got != (dbbranch.Limits{}) {
		t.Errorf("default = %+v, want no limit", got)
	}
	refQuotaGB.Store(2)
	maxPerUser.Store(5)
	maxBranchTTL.Store(int64(48 * time.Hour))
	t.Cleanup(func() { refQuotaGB.Store(0); maxPerUser.Store(0); maxBranchTTL.Store(0) })
	want := dbbranch.Limits{RefQuotaBytes: 2 << 30, MaxBranchesPerUser: 5, MaxTTL: 48 * time.Hour}
	if got := resourceLimits(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	keys := map[string]string{}
	for _, s := range coreSettings(config.Default()) {
		keys[s.Key] = s.Default
	}
	for _, k := range []string{"base.refresh_max_gb", "branch.refquota_gb", "branch.max_per_user", "branch.max_ttl", "cluster.join_token_ttl"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("setting %s is not declared", k)
		}
	}
}

func TestWireModulesAndSettings_clusterJoinTokenTTLを検証し反映する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st := &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")}
	svc := &dbbranch.Service{Store: st}
	srv := &httpapi.Server{Auth: &auth.Service{}}
	arc := &arcNode{ctl: &arccap.Controller{}}
	problems := &moduleProblems{m: map[string]string{}}

	prev := clusterJoinTokenTTL.Load()
	t.Cleanup(func() { clusterJoinTokenTTL.Store(prev) })

	wCtx, wCancel := context.WithCancel(ctx)
	_ = wireModulesAndSettings(wCtx, cfg, svc, srv, nil, nil, nil, nil,
		func() int { return 1 }, problems, arc, nil, nil, func() bool { return true }, discardLog())
	wCancel()

	if got := time.Duration(clusterJoinTokenTTL.Load()); got != 24*time.Hour {
		t.Fatalf("default clusterJoinTokenTTL = %v, want 24h", got)
	}
	for _, bad := range []string{"0s", "-1h", "soon"} {
		if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"cluster.join_token_ttl": bad}); !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("Put(cluster.join_token_ttl=%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"cluster.join_token_ttl": "2h"}); err != nil {
		t.Fatalf("Put valid cluster.join_token_ttl: %v", err)
	}
	wCtx2, wCancel2 := context.WithCancel(ctx)
	_ = wireModulesAndSettings(wCtx2, cfg, svc, srv, nil, nil, nil, nil,
		func() int { return 1 }, problems, arc, nil, nil, func() bool { return true }, discardLog())
	wCancel2()
	if got := time.Duration(clusterJoinTokenTTL.Load()); got != 2*time.Hour {
		t.Errorf("updated clusterJoinTokenTTL = %v, want 2h", got)
	}
}

func TestModuleSyncer_連続失敗で上限つき指数バックオフし継続時にイベントと問題を出し復旧で消す(t *testing.T) {
	ctx := context.Background()
	bus := operation.NewBus(16)
	sub := bus.Subscribe(0)
	defer sub.Close()

	var changeCalls int
	problems := &moduleProblems{m: map[string]string{}, onChange: func() { changeCalls++ }}

	var mu sync.Mutex
	var notices []string
	var syncErr error
	var syncRes module.SyncResult

	syncer := &moduleSyncer{
		adopt:         func(context.Context) error { return nil },
		sync:          func(context.Context) (module.SyncResult, error) { return syncRes, syncErr },
		problems:      problems,
		node:          "node1",
		bus:           bus,
		log:           discardLog(),
		baseDelay:     30 * time.Second,
		maxBackoff:    30 * time.Minute,
		kickDelay:     time.Millisecond,
		failThreshold: 2,
		notify: func(_ context.Context, event, level, subject, text string) {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, event+":"+level+":"+subject+":"+text)
		},
	}

	syncErr = errors.New("apt exit 100")
	syncRes = module.SyncResult{
		Failed: []module.FailedModule{{Name: "proxysql", Package: "db-branch-proxysql", Op: "install", Err: syncErr}},
	}

	// 1st failure: backs off to 1m, no persistent-failure problem/event yet.
	if w := syncer.step(ctx); w != 1*time.Minute {
		t.Fatalf("1st failure wait = %v, want 1m", w)
	}
	if p := problems.all(); len(p) != 0 {
		t.Fatalf("1st failure set problem prematurely: %v", p)
	}

	// Expected exponential backoff sequence capped at 30m.
	wantWaits := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, want := range wantWaits {
		if w := syncer.step(ctx); w != want {
			t.Fatalf("failure %d wait = %v, want %v", i+2, w, want)
		}
	}

	// Persistent failure recorded in moduleProblems (for /v1/modules & dbbranch_module_problem) and emitted once.
	p := problems.all()
	if !strings.Contains(p["proxysql"], "install failed") {
		t.Fatalf("problems = %v, want proxysql sync failure", p)
	}
	// Configure succeeding must not wipe sync problem.
	problems.set("proxysql", "")
	if p2 := problems.all(); !strings.Contains(p2["proxysql"], "install failed") {
		t.Fatalf("configure cleared sync problem: %v", p2)
	}

	mu.Lock()
	if len(notices) != 1 || !strings.HasPrefix(notices[0], "module.sync_failed:error:proxysql:") {
		t.Errorf("notices = %v, want single module.sync_failed", notices)
	}
	mu.Unlock()

	var sseTypes []string
	for len(sub.Events()) > 0 {
		ev := <-sub.Events()
		sseTypes = append(sseTypes, ev.Type)
	}
	if len(sseTypes) != 1 || sseTypes[0] != "module.sync_failed" {
		t.Errorf("SSE events = %v, want [module.sync_failed]", sseTypes)
	}

	// Recovery resets backoff to baseDelay and clears the sync problem.
	syncErr = nil
	syncRes = module.SyncResult{}
	if w := syncer.step(ctx); w != 30*time.Second {
		t.Fatalf("recovery wait = %v, want 30s", w)
	}
	if p := problems.all(); len(p) != 0 {
		t.Fatalf("problems after recovery = %v, want empty", p)
	}
	if changeCalls == 0 {
		t.Error("onChange was not called")
	}
}

func TestModuleSyncer_先行版はバックオフせず警告とイベントと問題で知らせ解決で消す(t *testing.T) {
	ctx := context.Background()
	bus := operation.NewBus(16)
	sub := bus.Subscribe(0)
	defer sub.Close()

	problems := &moduleProblems{m: map[string]string{}}
	var notices []string
	res := module.SyncResult{
		Ahead: []module.AheadModule{{
			Name:      "proxysql",
			Package:   "db-branch-proxysql",
			Installed: "0.3.0",
			Recorded:  "0.0.0~dev20261122011940.854bd9e",
		}},
	}

	syncer := &moduleSyncer{
		adopt:         func(context.Context) error { return nil },
		sync:          func(context.Context) (module.SyncResult, error) { return res, nil },
		problems:      problems,
		node:          "node1",
		bus:           bus,
		log:           discardLog(),
		baseDelay:     30 * time.Second,
		maxBackoff:    30 * time.Minute,
		kickDelay:     time.Millisecond,
		failThreshold: 2,
		notify: func(_ context.Context, event, level, subject, text string) {
			notices = append(notices, event+":"+level+":"+subject)
		},
	}

	// Ahead does not back off and emits event once across repeated steps.
	for range 3 {
		if w := syncer.step(ctx); w != 30*time.Second {
			t.Fatalf("ahead wait = %v, want 30s", w)
		}
	}
	if p := problems.all(); !strings.Contains(p["proxysql"], "0.3.0") || !strings.Contains(p["proxysql"], "0.0.0~dev") {
		t.Fatalf("problems = %v", p)
	}
	if len(notices) != 1 || notices[0] != "module.version_ahead:warning:proxysql" {
		t.Errorf("notices = %v, want single module.version_ahead", notices)
	}
	if len(sub.Events()) != 1 || (<-sub.Events()).Type != "module.version_ahead" {
		t.Errorf("expected single module.version_ahead SSE event")
	}

	// Once cluster record aligns, problem clears.
	res.Ahead = nil
	syncer.step(ctx)
	if p := problems.all(); len(p) != 0 {
		t.Errorf("problems after aligned = %v, want empty", p)
	}
}

func TestModuleSyncer_kickでバックオフをリセットして即座に再実行する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls sync.WaitGroup
	calls.Add(2)
	var count int
	var mu sync.Mutex
	timers := make(chan chan time.Time, 4)

	syncer := &moduleSyncer{
		adopt: func(context.Context) error { return nil },
		sync: func(context.Context) (module.SyncResult, error) {
			mu.Lock()
			count++
			c := count
			mu.Unlock()
			calls.Done()
			if c == 1 {
				return module.SyncResult{}, errors.New("apt failed")
			}
			return module.SyncResult{}, nil
		},
		problems:      &moduleProblems{m: map[string]string{}},
		log:           discardLog(),
		baseDelay:     30 * time.Second,
		maxBackoff:    30 * time.Minute,
		kickDelay:     0,
		failThreshold: 2,
		after: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			timers <- ch
			return ch
		},
	}

	kick := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		syncer.run(ctx, kick)
		close(done)
	}()

	// Wait for 1st step (failed -> waiting on timer).
	<-timers
	kick <- struct{}{}
	// Kick triggers immediate kickDelay timer, then 2nd step runs.
	ch := <-timers
	ch <- time.Now()
	calls.Wait()
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if syncer.backoffFailures != 0 || count < 2 {
		t.Errorf("backoffFailures=%d count=%d, want reset and >=2 runs", syncer.backoffFailures, count)
	}
}

func TestModuleSyncer_kick後も継続失敗はフラッピングせずモジュール別に数えBusyと混在時も維持する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	problems := &moduleProblems{m: map[string]string{}}
	var notifyCount int
	var syncRes module.SyncResult
	var syncErr error

	syncer := &moduleSyncer{
		adopt:         func(context.Context) error { return nil },
		sync:          func(context.Context) (module.SyncResult, error) { return syncRes, syncErr },
		problems:      problems,
		node:          "node1",
		log:           discardLog(),
		baseDelay:     30 * time.Second,
		maxBackoff:    30 * time.Minute,
		kickDelay:     0,
		failThreshold: 2,
		notify:        func(context.Context, string, string, string, string) { notifyCount++ },
	}

	// 1. Module A fails once, then recovers while B fails once -> B must not fire failThreshold on its 1st failure.
	syncErr = errors.New("err A")
	syncRes = module.SyncResult{Failed: []module.FailedModule{{Name: "a", Op: "install", Err: syncErr}}}
	syncer.step(ctx)
	syncErr = errors.New("err B")
	syncRes = module.SyncResult{Failed: []module.FailedModule{{Name: "b", Op: "install", Err: syncErr}}}
	syncer.step(ctx)
	if p := problems.all(); len(p) != 0 {
		t.Fatalf("B fired on its 1st failure: %v", p)
	}

	// 2. B fails 2nd time with Ahead module also present -> both B and Ahead appear, single notification.
	syncErr = errors.New("err B run 2") // different error text on same op must not re-notify
	syncRes = module.SyncResult{
		Ahead:  []module.AheadModule{{Name: "proxysql", Package: "db-branch-proxysql", Installed: "0.3.0", Recorded: "0.2.7"}},
		Failed: []module.FailedModule{{Name: "b", Op: "install", Err: syncErr}},
	}
	syncer.step(ctx)
	p := problems.all()
	if !strings.Contains(p["b"], "failed after 2 attempts") || !strings.Contains(p["proxysql"], "0.3.0") {
		t.Fatalf("mixed Ahead + Failed problems = %v", p)
	}
	if notifyCount != 2 { // 1 for proxysql ahead + 1 for b sync_failed
		t.Fatalf("notifyCount = %d, want 2", notifyCount)
	}

	// 3. Simulating kick reset (backoffFailures = 0) while B still fails must NOT clear B's problem or re-notify.
	syncer.backoffFailures = 0
	syncErr = errors.New("err B run 3")
	syncRes = module.SyncResult{
		Ahead:  []module.AheadModule{{Name: "proxysql", Package: "db-branch-proxysql", Installed: "0.3.0", Recorded: "0.2.7"}},
		Failed: []module.FailedModule{{Name: "b", Op: "install", Err: syncErr}},
	}
	if w := syncer.step(ctx); w != 1*time.Minute {
		t.Fatalf("wait after kick backoff reset = %v, want 1m", w)
	}
	if p := problems.all(); !strings.Contains(p["b"], "failed after 3 attempts") {
		t.Fatalf("problem flapped after kick: %v", p)
	}
	if notifyCount != 2 {
		t.Fatalf("re-notified on ongoing failure with varying error text: %d", notifyCount)
	}

	// 4. Busy (Skipped: true) preserves all problems and failure counts.
	syncErr = nil
	syncRes = module.SyncResult{Skipped: true}
	syncer.step(ctx)
	if p := problems.all(); !strings.Contains(p["b"], "failed after 3 attempts") || !strings.Contains(p["proxysql"], "0.3.0") {
		t.Fatalf("Busy cleared problems: %v", p)
	}

	// 5. Store-level failure (res.Failed empty) records under _sync after threshold.
	syncErr = errors.New("raft unavailable")
	syncRes = module.SyncResult{}
	syncer.step(ctx) // count 1
	syncer.step(ctx) // count 2
	if p := problems.all(); !strings.Contains(p["_sync"], "raft unavailable") {
		t.Fatalf("_sync fallback missing: %v", p)
	}
}

func TestModuleKicks_moduleUpdatedイベントだけで起動する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := operation.NewBus(10)
	var once sync.Once
	var kicked sync.WaitGroup
	kicked.Add(1)
	moduleKicks(ctx, bus, func() { once.Do(func() { kicked.Done() }) })
	bus.Publish("module.sync_failed", nil)
	bus.Publish("module.updated", map[string]string{"name": "proxysql"})
	done := make(chan struct{})
	go func() { kicked.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("module.updated did not trigger kick")
	}
}

func TestModuleStore_authモジュールがuser削除時に個人トークンを失効させる(t *testing.T) {
	ctx := context.Background()
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	toks := &token.Service{Store: st, Clock: time.Now}
	alice := ext.Principal{ID: "user:alice@example.com", Name: "alice@example.com"}
	bob := ext.Principal{ID: "user:bob@example.com", Name: "bob@example.com"}
	if _, _, err := toks.CreatePersonal(ctx, alice, "mcp", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := toks.CreatePersonal(ctx, bob, "mcp", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := toks.Create(ctx, "ci", domain.ScopeBranches, 0); err != nil {
		t.Fatal(err)
	}

	authStore := moduleStoreFor("auth", st, toks, discardLog())
	auditStore := moduleStoreFor("audit", st, toks, discardLog())
	_ = authStore.Put(ctx, "user/alice@example.com", []byte("{}"))
	_ = authStore.Put(ctx, "team/devs", []byte("{}"))
	_ = auditStore.Put(ctx, "user/bob@example.com", []byte("{}"))

	// Deleting from audit or deleting a non-user key in auth leaves tokens untouched.
	if err := auditStore.Delete(ctx, "user/bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := authStore.Delete(ctx, "team/devs"); err != nil {
		t.Fatal(err)
	}
	if list, _ := toks.List(ctx); len(list) != 3 {
		t.Fatalf("unexpected revoke: %+v", list)
	}

	// Deleting user/alice@example.com from auth revokes only Alice's personal tokens.
	if err := authStore.Delete(ctx, "user/alice@example.com"); err != nil {
		t.Fatal(err)
	}
	aliceToks, _ := toks.ListOwned(ctx, alice.ID)
	bobToks, _ := toks.ListOwned(ctx, bob.ID)
	svcToks, _ := toks.ListService(ctx)
	if len(aliceToks) != 0 || len(bobToks) != 1 || len(svcToks) != 1 {
		t.Errorf("after auth user delete: alice=%d bob=%d svc=%d", len(aliceToks), len(bobToks), len(svcToks))
	}
}

func TestSweepExpiredTokens_クラスタではリーダーだけがGCを実行する(t *testing.T) {
	ctx := context.Background()
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	toks := &token.Service{Store: st, Clock: func() time.Time { return now }}
	if _, _, err := toks.Create(ctx, "short", domain.ScopeBranches, time.Hour); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)

	// Follower does not sweep.
	sweepExpiredTokens(ctx, toks, func() bool { return false }, discardLog())
	if list, _ := toks.List(ctx); len(list) != 1 {
		t.Fatalf("follower swept tokens: %+v", list)
	}

	// Leader sweeps expired tokens.
	sweepExpiredTokens(ctx, toks, func() bool { return true }, discardLog())
	if list, _ := toks.List(ctx); len(list) != 0 {
		t.Fatalf("leader did not sweep tokens: %+v", list)
	}
}

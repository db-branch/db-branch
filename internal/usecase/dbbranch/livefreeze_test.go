package dbbranch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// freezeProvider is the runtime provider of an engine that wants its server
// readied for a live checkpoint (like ClickHouse stopping merges).
type freezeProvider struct {
	domain.RuntimeProvider
	j        *journal
	liveFrz  bool
	preErr   error
	postErr  error
	insts    []domain.InstanceSpec
	describe error
	onPre    func() // runs inside PreFreeze (e.g. cancels the caller's context)
	preBlock bool   // PreFreeze waits until its context ends
	// postCtxErr is the state of the context PostFreeze got.
	postCtxErr error
}

func (p *freezeProvider) Describe(context.Context) (domain.EngineSpec, error) {
	spec, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	spec.Capabilities.LiveFreeze = p.liveFrz
	return spec, p.describe
}

func (p *freezeProvider) PreFreeze(ctx context.Context, s domain.InstanceSpec) (domain.FreezeToken, error) {
	p.j.add("prefreeze")
	p.insts = append(p.insts, s)
	if p.onPre != nil {
		p.onPre()
	}
	if p.preBlock {
		<-ctx.Done()
		return domain.FreezeToken{}, ctx.Err()
	}
	return domain.FreezeToken{Data: "tok"}, p.preErr
}

func (p *freezeProvider) PostFreeze(ctx context.Context, s domain.InstanceSpec, tok domain.FreezeToken) error {
	p.j.add("postfreeze %s", tok.Data)
	p.postCtxErr = ctx.Err()
	return p.postErr
}

func (z *zenv) freezing(p *freezeProvider) *freezeProvider {
	p.j = z.j
	z.svc.Providers = domain.ProviderSet{domain.MySQL: p}
	return p
}

// at is the position of the first journal entry that starts with prefix, or -1.
func (z *zenv) at(prefix string) int {
	for i, e := range z.j.entries {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func (z *zenv) freezeIntent(t *testing.T) string {
	t.Helper()
	props, err := z.sim.Props(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	return props[domain.PropFreeze]
}

func TestCreateCheckpoint_ライブ凍結を宣言したエンジンは意図を記録し凍結してからスナップショットして解除する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	p := z.freezing(&freezeProvider{liveFrz: true})

	z.checkpoint(t, "c1")

	order := []int{z.at("zfs set db-branch:freeze=on " + live), z.at("prefreeze"), z.at("zfs snapshot " + live + "@"), z.at("postfreeze tok"), z.at("zfs inherit [db-branch:freeze] " + live)}
	for i, pos := range order {
		if pos < 0 || (i > 0 && pos < order[i-1]) {
			t.Fatalf("order = %v in %q: want intent, prefreeze, snapshot, postfreeze, intent cleared", order, z.j.entries)
		}
	}
	if got := z.freezeIntent(t); got != "" {
		t.Errorf("freeze intent left behind: %q", got)
	}
	info, _ := z.sim.Info(ctx, live)
	if len(p.insts) != 1 || p.insts[0].DataDir == "" || p.insts[0].DataDir != info.Mountpoint {
		t.Errorf("DataDir given to PreFreeze = %+v, want the dataset's mount point %q", p.insts, info.Mountpoint)
	}
	if len(p.insts) != 1 || p.insts[0].ID != "branch-x" || p.insts[0].Runtime != mysql84 || p.insts[0].Ports["mysql"] == 0 {
		t.Errorf("instance given to PreFreeze = %+v", p.insts)
	}
}

func TestCreateCheckpoint_PreFreezeが失敗したらスナップショットせずPostFreezeで必ず戻す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true, preErr: errors.New("merges would not stop")})

	z.tick()
	_, err := z.svc.CreateCheckpoint(ctx, name_("x"), "c1")
	if err == nil || !strings.Contains(err.Error(), "merges would not stop") {
		t.Fatalf("CreateCheckpoint = %v", err)
	}
	if z.at("zfs snapshot "+live+"@") >= 0 {
		t.Errorf("a snapshot was taken after PreFreeze failed: %q", z.j.entries)
	}
	if z.at("postfreeze") < 0 {
		t.Errorf("PostFreeze was not called to undo a partly applied freeze: %q", z.j.entries)
	}
	if got := z.freezeIntent(t); got != "" {
		t.Errorf("freeze intent left behind: %q", got)
	}
}

func TestCreateCheckpoint_PostFreezeが失敗したら意図を残してエラーを返す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true, postErr: errors.New("merges would not start")})

	z.tick()
	_, err := z.svc.CreateCheckpoint(ctx, name_("x"), "c1")
	if err == nil || !strings.Contains(err.Error(), "merges would not start") {
		t.Fatalf("CreateCheckpoint = %v", err)
	}
	if got := z.freezeIntent(t); got != "on" {
		t.Errorf("freeze intent = %q, want it kept so recovery releases the freeze", got)
	}
}

func TestCreateCheckpoint_ライブ凍結を宣言しないエンジンでは何もしない(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: false})
	z.checkpoint(t, "c1")
	if z.at("prefreeze") >= 0 || z.at("postfreeze") >= 0 || z.at("zfs set db-branch:freeze") >= 0 {
		t.Errorf("freeze steps for an engine without live_freeze: %q", z.j.entries)
	}

	y := newZEnv(t) // no provider at all
	y.branch(t)
	y.checkpoint(t, "c1")
	if y.at("zfs set db-branch:freeze") >= 0 {
		t.Errorf("freeze intent without a provider: %q", y.j.entries)
	}
}

func TestCreateCheckpoint_プロバイダの宣言を読めなければ失敗しスナップショットしない(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{describe: errors.New("provider down")})
	z.tick()
	if _, err := z.svc.CreateCheckpoint(ctx, name_("x"), "c1"); err == nil {
		t.Error("CreateCheckpoint succeeded")
	}
	if z.at("zfs snapshot "+live+"@") >= 0 {
		t.Errorf("snapshot taken: %q", z.j.entries)
	}
}

func TestRecover_凍結の意図が残っていれば起動後にPostFreezeで解除して意図を消す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	p := z.freezing(&freezeProvider{liveFrz: true})
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z.restart() // the agent died with the server: it is down when recovery starts
	z.j.entries = nil

	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	start, post := z.at("start "), z.at("postfreeze ")
	if post < 0 || start < 0 || post < start {
		t.Errorf("want the server started, then PostFreeze: %q", z.j.entries)
	}
	if got := z.freezeIntent(t); got != "" {
		t.Errorf("freeze intent left behind: %q", got)
	}
	if len(p.insts) != 0 {
		t.Errorf("Recover must not freeze: %+v", p.insts)
	}
}

func TestRecover_意図がなければPostFreezeを呼ばない(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	z.j.entries = nil
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if z.at("postfreeze") >= 0 {
		t.Errorf("PostFreeze without an intent: %q", z.j.entries)
	}
}

func TestCreateCheckpoint_呼び出し元のctxが取り消されてもPostFreezeと意図の解除は続ける(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	caller, cancel := context.WithCancel(ctx)
	p := z.freezing(&freezeProvider{liveFrz: true, onPre: cancel})

	z.tick()
	_, _ = z.svc.CreateCheckpoint(caller, name_("x"), "c1")
	if z.at("postfreeze") < 0 {
		t.Fatalf("PostFreeze was not called: %q", z.j.entries)
	}
	if p.postCtxErr != nil {
		t.Errorf("PostFreeze got a cancelled context: %v", p.postCtxErr)
	}
	if got := z.freezeIntent(t); got != "" {
		t.Errorf("freeze intent left behind: %q", got)
	}
}

func TestCreateCheckpoint_PreFreezeには時間の上限がありタイムアウトしたら諦めてPostFreezeで戻す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.svc.FreezeTimeout = 20 * time.Millisecond
	p := z.freezing(&freezeProvider{liveFrz: true, preBlock: true})
	caller, cancel := context.WithTimeout(ctx, 3*time.Second) // only so a missing limit fails the test instead of hanging it
	defer cancel()

	z.tick()
	start := time.Now()
	_, err := z.svc.CreateCheckpoint(caller, name_("x"), "c1")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("CreateCheckpoint = %v after %s, want the 20ms freeze limit to end it", err, time.Since(start))
	}
	if z.at("zfs snapshot "+live+"@") >= 0 {
		t.Errorf("snapshot taken after the freeze timed out: %q", z.j.entries)
	}
	if z.at("postfreeze") < 0 || p.postCtxErr != nil {
		t.Errorf("PostFreeze must still run, with a live context (err %v): %q", p.postCtxErr, z.j.entries)
	}
}

func TestCreateCheckpoint_意図を書けなければPreFreezeを呼ばない(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	z.crash.at = z.crash.steps + 1 // the very next change to the disk fails: the intent

	z.tick()
	if _, err := z.svc.CreateCheckpoint(ctx, name_("x"), "c1"); err == nil {
		t.Fatal("CreateCheckpoint succeeded")
	}
	if z.at("prefreeze") >= 0 || z.at("postfreeze") >= 0 {
		t.Errorf("the server was frozen without a recorded intent: %q", z.j.entries)
	}
}

func TestRecover_ユーザーの是正に失敗しても凍結の解除は行う(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z.server.failOn = "exec"
	z.j.entries = nil

	if err := z.svc.Recover(ctx); err == nil {
		t.Fatal("Recover reported no failure")
	}
	if z.at("postfreeze ") < 0 {
		t.Errorf("the freeze was not released: %q", z.j.entries)
	}
}

func TestRecover_PostFreezeに失敗したら意図を残す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true, postErr: errors.New("still frozen")})
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	if err := z.svc.Recover(ctx); err == nil || !strings.Contains(err.Error(), "still frozen") {
		t.Fatalf("Recover = %v", err)
	}
	if got := z.freezeIntent(t); got != "on" {
		t.Errorf("freeze intent = %q, want it kept for the next recovery", got)
	}
}

func TestRecover_サーバー起動後のユーザー是正に失敗しても凍結は解除する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z.restart() // down: startIfDown starts it, then putting the login right fails
	z.server.failOn = "exec"
	z.j.entries = nil

	if err := z.svc.Recover(ctx); err == nil {
		t.Fatal("Recover reported no failure")
	}
	if z.at("postfreeze ") < 0 {
		t.Errorf("the freeze was not released although the server was started: %q", z.j.entries)
	}
}

func TestReconcile_失われていたブランチを再開するときも凍結の意図が残っていれば解除する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	br, err := z.store.GetBranch(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	br.Status = domain.BranchLost
	if err := z.store.PutBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z.j.entries = nil

	if err := z.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if z.at("postfreeze ") < 0 {
		t.Errorf("the freeze was not released: %q", z.j.entries)
	}
	if got := z.freezeIntent(t); got != "" {
		t.Errorf("freeze intent left behind: %q", got)
	}
}

func TestReconcile_凍結を解除できなければ再開せず失われたままにして次の回で再試行する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	fp := z.freezing(&freezeProvider{liveFrz: true, postErr: errors.New("provider not ready")})
	br, _ := z.store.GetBranch(ctx, name("x"))
	br.Status = domain.BranchLost
	if err := z.store.PutBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}

	if err := z.svc.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile reported no failure")
	}
	if got, _ := z.store.GetBranch(ctx, name("x")); got.Status != domain.BranchLost {
		t.Errorf("status = %s, want it to stay lost while the freeze is not released", got.Status)
	}
	fp.postErr = nil
	if err := z.svc.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile = %v", err)
	}
	if got, _ := z.store.GetBranch(ctx, name("x")); got.Status != domain.BranchReady {
		t.Errorf("status after the retry = %s", got.Status)
	}
}

func TestRecover_BranchReadyでownUserより先にreleaseFreezeが呼ばれる(t *testing.T) {
	// Case 1: server already running, legacy shared user -> postfreeze must precede ownUser and putLoginRight.
	z := newZEnv(t)
	z.branch(t)
	z.freezing(&freezeProvider{liveFrz: true})
	br, err := z.store.GetBranch(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	br.User = "branch" // dbbranch.BranchUser
	if err := z.store.PutBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z.j.entries = nil
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	post, exec := z.at("postfreeze "), z.at("exec branch-x")
	if post < 0 || exec < 0 || post > exec {
		t.Errorf("running branch: want postfreeze before exec (ownUser/putLoginRight), got %q", z.j.entries)
	}

	// Case 2: server was down -> start < postfreeze < exec (putLoginRight).
	z2 := newZEnv(t)
	z2.branch(t)
	z2.freezing(&freezeProvider{liveFrz: true})
	if err := z2.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z2.restart()
	z2.j.entries = nil
	if err := z2.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	start2, post2, exec2 := z2.at("start "), z2.at("postfreeze "), z2.at("exec branch-x")
	if start2 < 0 || post2 < 0 || exec2 < 0 || !(start2 < post2 && post2 < exec2) {
		t.Errorf("stopped branch: want start < postfreeze < exec, got %q", z2.j.entries)
	}

	// Case 3: Reconcile on a lost branch whose server was down -> start < postfreeze < exec.
	z3 := newZEnv(t)
	z3.branch(t)
	z3.freezing(&freezeProvider{liveFrz: true})
	br3, _ := z3.store.GetBranch(ctx, name("x"))
	br3.Status = domain.BranchLost
	if err := z3.store.PutBranch(ctx, br3); err != nil {
		t.Fatal(err)
	}
	if err := z3.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"}); err != nil {
		t.Fatal(err)
	}
	z3.restart()
	z3.j.entries = nil
	if err := z3.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	start3, post3, exec3 := z3.at("start "), z3.at("postfreeze "), z3.at("exec branch-x")
	if start3 < 0 || post3 < 0 || exec3 < 0 || !(start3 < post3 && post3 < exec3) {
		t.Errorf("Reconcile stopped lost branch: want start < postfreeze < exec, got %q", z3.j.entries)
	}

	// Case 4: Recover skips ownUser/putLoginRight when releaseFreeze fails (prevents deadlock on frozen server).
	z4 := newZEnv(t)
	z4.branch(t)
	z4.freezing(&freezeProvider{liveFrz: true, postErr: errors.New("still frozen")})
	br4, _ := z4.store.GetBranch(ctx, name("x"))
	br4.User = "branch"
	_ = z4.store.PutBranch(ctx, br4)
	_ = z4.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"})
	z4.j.entries = nil
	if err := z4.svc.Recover(ctx); err == nil {
		t.Fatal("Case 4: want error when PostFreeze fails")
	}
	if z4.at("exec branch-x") >= 0 {
		t.Errorf("Case 4: ran ownUser/putLoginRight while still frozen: %q", z4.j.entries)
	}

	// Case 5: Recover on a running frozen recorded branch with missing history module still releases freeze.
	z5 := newZEnv(t)
	z5.branch(t)
	z5.freezing(&freezeProvider{liveFrz: true})
	br5, _ := z5.store.GetBranch(ctx, name("x"))
	br5.Record = true
	// Record is fixed at creation, so replace the stored branch instead of updating it.
	_ = z5.store.DeleteBranch(ctx, name("x"))
	_ = z5.store.PutBranch(ctx, br5)
	_ = z5.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"})
	z5.svc.ServerOptions = nil // causes ErrRecordRequired in branchSpec
	z5.j.entries = nil
	if err := z5.svc.Recover(ctx); err == nil {
		t.Fatal("Case 5: want ErrRecordRequired")
	}
	if z5.at("postfreeze ") < 0 || z5.freezeIntent(t) != "" {
		t.Errorf("Case 5: freeze was not released on running branch despite ErrRecordRequired: intent=%q journal=%q", z5.freezeIntent(t), z5.j.entries)
	}

	// Case 6: retryRecorded releases leftover freeze before putLoginRight once the module returns.
	z6 := newZEnv(t)
	z6.options()
	z6.branch(t)
	z6.freezing(&freezeProvider{liveFrz: true})
	br6, _ := z6.store.GetBranch(ctx, name("x"))
	br6.Record = true
	br6.Status = domain.BranchFailed
	br6.Error = "this branch records its changes (decided when it was created) and does not start without recording: missing"
	_ = z6.store.DeleteBranch(ctx, name("x"))
	_ = z6.store.PutBranch(ctx, br6)
	_ = z6.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"})
	z6.restart()
	z6.j.entries = nil
	if err := z6.svc.Recover(ctx); err != nil {
		t.Fatalf("Case 6: Recover = %v", err)
	}
	start6, post6, exec6 := z6.at("start "), z6.at("postfreeze "), z6.at("exec branch-x")
	if start6 < 0 || post6 < 0 || exec6 < 0 || !(start6 < post6 && post6 < exec6) {
		t.Errorf("Case 6 retryRecorded: want start < postfreeze < exec, got %q", z6.j.entries)
	}
}

// A server started only to put its login right is stopped again when that
// work fails, so a failed recovery leaves no process behind.
func TestRecover_起動した復旧がfreeze解除かログイン修復で失敗したらサーバーを停止する(t *testing.T) {
	recordedFailed := func(z *zenv) {
		br, _ := z.store.GetBranch(ctx, name("x"))
		br.Record = true
		br.Status = domain.BranchFailed
		br.Error = "this branch records its changes (decided when it was created) and does not start without recording: missing"
		_ = z.store.DeleteBranch(ctx, name("x"))
		_ = z.store.PutBranch(ctx, br)
	}
	lost := func(z *zenv) {
		br, _ := z.store.GetBranch(ctx, name("x"))
		br.Status = domain.BranchLost
		_ = z.store.PutBranch(ctx, br)
	}
	recover := func(z *zenv) error { return z.svc.Recover(ctx) }
	reconcile := func(z *zenv) error { return z.svc.Reconcile(ctx) }
	cases := []struct {
		name  string
		setup func(*zenv)
		run   func(*zenv) error
		prov  *freezeProvider
		fail  string
	}{
		{"retryRecorded freeze解除失敗", recordedFailed, recover, &freezeProvider{liveFrz: true, postErr: errors.New("frozen")}, ""},
		{"retryRecorded ログイン修復失敗", recordedFailed, recover, &freezeProvider{liveFrz: true}, "exec"},
		{"reconcile freeze解除失敗", lost, reconcile, &freezeProvider{liveFrz: true, postErr: errors.New("frozen")}, ""},
		{"reconcile ログイン修復失敗", lost, reconcile, &freezeProvider{liveFrz: true}, "exec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z := newZEnv(t)
			z.options()
			z.branch(t)
			z.freezing(c.prov)
			c.setup(z)
			_ = z.sim.SetProps(ctx, live, map[string]string{domain.PropFreeze: "on"})
			z.restart()
			z.server.failOn = c.fail
			z.j.entries = nil
			if err := c.run(z); err == nil {
				t.Fatal("want an error")
			}
			if z.at("start ") < 0 || z.at("stop branch-x immediate") < 0 {
				t.Errorf("want the started server stopped, got %q", z.j.entries)
			}
		})
	}
}

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

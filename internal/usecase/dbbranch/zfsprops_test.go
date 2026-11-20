package dbbranch_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// engineWithProps makes the test's registry say mysql wants the given ZFS properties.
func engineWithProps(t *testing.T, props map[string]string) {
	t.Helper()
	old := domain.DefaultRuntimes
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: domain.MySQL, ZFSProps: props}); err != nil {
		t.Fatal(err)
	}
	domain.DefaultRuntimes = reg
	t.Cleanup(func() { domain.DefaultRuntimes = old })
}

func TestCreateBase_エンジンの推奨ZFSプロパティのうち親と違うものだけをデータセットに設定する(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K", "compression": "lz4", "atime": "off"})
	e := newEnv(t)
	e.withBase(t)

	want := map[string]string{"recordsize": "128K"} // compression and atime are what the parent already has
	if got := e.storage.props["tank/db-branch/bases/prod"]; !reflect.DeepEqual(got, want) {
		t.Errorf("props = %v, want %v", got, want)
	}
}

func TestCreateBase_組み込みエンジンの推奨値は親と同じなので何も設定せず継承する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	if got := e.storage.props["tank/db-branch/bases/prod"]; len(got) != 0 {
		t.Errorf("props = %v, want none (inherited from the parent)", got)
	}
}

// setPropsRecorder is the part of CheckpointStorage that receiving a copy needs.
type setPropsRecorder struct {
	domain.CheckpointStorage
	sets map[string]map[string]string
	err  error
}

func (r *setPropsRecorder) SetProps(_ context.Context, target string, props map[string]string) error {
	if r.sets == nil {
		r.sets = map[string]map[string]string{}
	}
	r.sets[target] = props
	return r.err
}

func TestReplicate_受信したコピーにもエンジンの推奨ZFSプロパティを設定する(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	p := newPair(t)
	rec := &setPropsRecorder{}
	p.replica.svc.Lineage = rec
	p.withBase(t)

	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"recordsize": "128K"}
	if got := rec.sets["tank/db-branch/replicas/prod"]; !reflect.DeepEqual(got, want) {
		t.Errorf("props set on the received copy = %v, want %v", got, want)
	}
	if len(rec.sets) != 1 {
		t.Errorf("props set on %v", rec.sets)
	}
}

func TestReplicate_組み込みエンジンや履歴を持たない保管先では何も設定しない(t *testing.T) {
	p := newPair(t)
	rec := &setPropsRecorder{}
	p.replica.svc.Lineage = rec
	p.withBase(t)
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.sets) != 0 {
		t.Errorf("props set for a built-in engine: %v", rec.sets)
	}

	engineWithProps(t, map[string]string{"recordsize": "128K"})
	q := newPair(t) // no Lineage: storage without ZFS properties to set
	q.withBase(t)
	if err := q.replica.svc.Replicate(ctx); err != nil {
		t.Errorf("Replicate without Lineage = %v", err)
	}
}

func TestReplicate_プロパティを設定できなければ失敗を返す(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	p := newPair(t)
	p.replica.svc.Lineage = &setPropsRecorder{err: errors.New("zfs set failed")}
	p.withBase(t)
	if err := p.replica.svc.Replicate(ctx); err == nil || !strings.Contains(err.Error(), "zfs set failed") {
		t.Errorf("Replicate = %v", err)
	}
}

func TestReplicate_プロパティを設定できなかったコピーは捨てて次の回で最初からやり直す(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	p := newPair(t)
	rec := &setPropsRecorder{err: errors.New("zfs set failed")}
	p.replica.svc.Lineage = rec
	p.withBase(t)
	replica := "tank/db-branch/replicas/prod"

	if err := p.replica.svc.Replicate(ctx); err == nil {
		t.Fatal("Replicate succeeded")
	}
	if p.replica.storage.datasets[replica] {
		t.Error("the copy without the engine's properties was kept")
	}

	rec.err = nil
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatalf("second Replicate = %v", err)
	}
	want := map[string]string{"recordsize": "128K"}
	if got := rec.sets[replica]; !reflect.DeepEqual(got, want) || !p.replica.storage.datasets[replica] {
		t.Errorf("after the retry: props %v, copy present %v", got, p.replica.storage.datasets[replica])
	}
}

// A zfs clone takes the properties of its new parent, not of its origin, so
// each branch dataset gets the engine's own.

func TestCreateBranch_クローンにもエンジンの推奨ZFSプロパティを設定する(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K", "compression": "lz4"})
	z := newZEnv(t)
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil); err != nil {
		t.Fatal(err)
	}

	if !z.j.has("zfs set recordsize=128K " + live) {
		t.Errorf("the branch dataset did not get its recordsize: %q", z.j.entries)
	}
	for _, e := range z.j.entries {
		if strings.Contains(e, "compression") {
			t.Errorf("compression is what the parent already has, yet: %q", e)
		}
	}
	if props, _ := z.sim.Props(ctx, live); props["recordsize"] != "128K" {
		t.Errorf("recordsize of the branch dataset = %q, want 128K", props["recordsize"])
	}
}

func TestCreateBranch_組み込みエンジンのクローンには何も設定しない(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	if props, _ := z.sim.Props(ctx, live); props["recordsize"] != "" {
		t.Errorf("props = %v", props)
	}
}

func TestCreateBranch_プロパティを設定できなければブランチを作らない(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	e := newEnv(t)
	e.withBase(t)
	e.svc.Lineage = &setPropsRecorder{err: errors.New("zfs set failed")}

	_, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil)
	if err == nil || !strings.Contains(err.Error(), "zfs set failed") {
		t.Errorf("CreateBranch = %v", err)
	}
}

func TestRestoreCheckpoint_復元したラインにもエンジンの推奨ZFSプロパティを設定する(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	z := newZEnv(t)
	z.branch(t)
	z.checkpoint(t, "c1")
	z.sim.write(live, "v1")

	if _, err := z.svc.RestoreCheckpoint(ctx, name_("x"), "c1", nil); err != nil {
		t.Fatal(err)
	}
	props, err := z.sim.Props(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if props["recordsize"] != "128K" {
		t.Errorf("recordsize of the restored live dataset = %q, want 128K (props %v)", props["recordsize"], props)
	}
}

// cancelOnSetProps cancels the caller's context as it fails, like an agent being stopped.
type cancelOnSetProps struct {
	domain.CheckpointStorage
	cancel func()
}

func (c *cancelOnSetProps) SetProps(context.Context, string, map[string]string) error {
	c.cancel()
	return errors.New("interrupted")
}

func TestReplicate_プロパティ設定が中断されても後始末は取り消されないctxで行う(t *testing.T) {
	engineWithProps(t, map[string]string{"recordsize": "128K"})
	p := newPair(t)
	caller, cancel := context.WithCancel(ctx)
	p.replica.svc.Lineage = &cancelOnSetProps{cancel: cancel}
	p.withBase(t)

	if err := p.replica.svc.Replicate(caller); err == nil {
		t.Fatal("Replicate succeeded")
	}
	if p.replica.storage.destroyCtxErr != nil || p.replica.storage.datasets["tank/db-branch/replicas/prod"] {
		t.Errorf("the half-made copy was not removed with a live context (ctx err %v)", p.replica.storage.destroyCtxErr)
	}
}

func TestReplicate_プロパティが欠けた最新のコピーは次の回で設定し直す(t *testing.T) {
	p := newPair(t)
	rec := &setPropsRecorder{}
	p.replica.svc.Lineage = rec
	p.withBase(t)
	if err := p.replica.svc.Replicate(ctx); err != nil { // copied while the engine asked for nothing special
		t.Fatal(err)
	}
	if len(rec.sets) != 0 {
		t.Fatalf("props set early: %v", rec.sets)
	}

	engineWithProps(t, map[string]string{"recordsize": "128K"}) // e.g. the copy lost its properties in a crash
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"recordsize": "128K"}
	if got := rec.sets["tank/db-branch/replicas/prod"]; !reflect.DeepEqual(got, want) {
		t.Errorf("props set on the up-to-date copy = %v, want %v", got, want)
	}
}

package dbbranch_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
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

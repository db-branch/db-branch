package modmask_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/modmask"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

type masker struct{ got ext.MaskJob }

func (m *masker) Mask(_ context.Context, j ext.MaskJob) (string, error) { m.got = j; return "ok", nil }

type policy struct {
	plans []ext.MaskPlan
	snap  string
}

func (p *policy) PrepareMask(_ context.Context, pl ext.MaskPlan) (string, error) {
	p.plans = append(p.plans, pl)
	return "approved", nil
}
func (p *policy) VerifyMask(_ context.Context, pl ext.MaskPlan) error {
	p.plans = append(p.plans, pl)
	return nil
}
func (p *policy) MaskRecorded(_ context.Context, pl ext.MaskPlan, s string) error {
	p.snap = s
	return nil
}

var src = domain.AdminAccess{Engine: domain.Postgres, Host: "127.0.0.1", Socket: "/run/a", Port: 1, User: "postgres", BinDir: "/rt/bin", LibDir: "/rt/lib"}

func TestMasker_モジュールのマスカーへ型を移して渡す(t *testing.T) {
	m := &masker{}
	var d domain.Masker = modmask.Masker{M: m}
	log, err := d.Mask(ctx, domain.MaskJob{Source: src, Target: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/b"}, Rules: "r"})
	want := ext.DBAccess{Engine: "postgres", Host: "127.0.0.1", Socket: "/run/a", Port: 1, User: "postgres", BinDir: "/rt/bin", LibDir: "/rt/lib"}
	if err != nil || log != "ok" || !reflect.DeepEqual(m.got.Source, want) || m.got.Target.Socket != "/run/b" || m.got.Rules != "r" {
		t.Errorf("%q %v %+v", log, err, m.got)
	}
}

func TestPolicy_計画を渡し承認されたルールと記録を返す(t *testing.T) {
	p := &policy{}
	var d domain.MaskPolicy = modmask.Policy{P: p}
	plan := domain.MaskPlan{Source: mustName("prod"), SourceSnapshot: "s1", Target: mustName("dev"), Masker: "greenmask", Rules: "mine", By: "ann", SourceDB: src}
	rules, err := d.PrepareMask(ctx, plan)
	if err != nil || rules != "approved" {
		t.Fatalf("%q %v", rules, err)
	}
	_ = d.VerifyMask(ctx, plan)
	_ = d.MaskRecorded(ctx, plan, "s9")
	got := p.plans[0]
	if got.Source != "prod" || got.Target != "dev" || got.SourceSnapshot != "s1" || got.Masker != "greenmask" || got.By != "ann" || got.SourceDB.Socket != "/run/a" || len(p.plans) != 2 || p.snap != "s9" {
		t.Errorf("%+v %q", p.plans, p.snap)
	}
}

func mustName(s string) domain.Name { n, _ := domain.ParseName(s); return n }

type follower struct {
	base, snap string
	err        error
}

func (f *follower) AfterRefresh(_ context.Context, base, snap string) ([]ext.MaskRun, error) {
	f.base, f.snap = base, snap
	return []ext.MaskRun{{Target: "dev", Masker: "greenmask"}}, f.err
}

func TestFollower_ベース名とスナップショットを渡し実行を型を移して返す(t *testing.T) {
	f := &follower{}
	var d domain.RefreshFollower = modmask.Follower{F: f}
	runs, err := d.AfterRefresh(ctx, mustName("prod"), "s1")
	if err != nil || len(runs) != 1 || runs[0] != (domain.MaskRun{Target: "dev", Masker: "greenmask"}) || f.base != "prod" || f.snap != "s1" {
		t.Errorf("%v %v %+v", runs, err, f)
	}
	f.err = ext.ErrForbidden
	if _, err := d.AfterRefresh(ctx, mustName("prod"), "s1"); err == nil {
		t.Error("an error is passed on")
	}
}

func TestMasker_複数ポートのエンドポイントと環境変数も渡し元とは別の写しにする(t *testing.T) {
	m := &masker{}
	a := src
	a.Endpoints = map[string]domain.Endpoint{"http": {Host: "127.0.0.1", Port: 8123}, "native": {Socket: "/run/n.sock"}}
	a.Env = map[string]string{"X": "1"}
	if _, err := (modmask.Masker{M: m}).Mask(ctx, domain.MaskJob{Source: a, Target: a, Rules: "r"}); err != nil {
		t.Fatal(err)
	}
	got := m.got.Source
	if got.Endpoints["http"] != (ext.Endpoint{Host: "127.0.0.1", Port: 8123}) || got.Endpoints["native"].Socket != "/run/n.sock" || got.Env["X"] != "1" {
		t.Errorf("access = %+v", got)
	}
	got.Env["X"] = "changed"
	got.Endpoints["http"] = ext.Endpoint{}
	if a.Env["X"] != "1" || a.Endpoints["http"].Port != 8123 {
		t.Error("the module's copy shares its maps with the agent's")
	}
}

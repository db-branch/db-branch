package dbbranch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

type fakeMasker struct {
	j    *journal
	jobs []domain.MaskJob
	err  error
}

func (m *fakeMasker) Mask(_ context.Context, job domain.MaskJob) (string, error) {
	m.j.add("mask %d -> %d", job.Source.Port, job.Target.Port)
	m.jobs = append(m.jobs, job)
	return "masked 3 tables", m.err
}

type fakePolicy struct {
	rules              string
	refuse, verifyFail error
	recorded           []string
	plans              []domain.MaskPlan
}

func (p *fakePolicy) PrepareMask(_ context.Context, plan domain.MaskPlan) (string, error) {
	p.plans = append(p.plans, plan)
	return p.rules, p.refuse
}
func (p *fakePolicy) VerifyMask(_ context.Context, plan domain.MaskPlan) error { return p.verifyFail }
func (p *fakePolicy) MaskRecorded(_ context.Context, plan domain.MaskPlan, snap string) error {
	p.recorded = append(p.recorded, plan.Target.String()+"@"+snap)
	return nil
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// maskEnv has base "prod" (the source) and an empty base "dev" (the target).
func maskEnv(t *testing.T) (*env, *fakeMasker) {
	t.Helper()
	e := newEnv(t)
	e.withBase(t)
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("dev"), Runtime: mysql84}, nil); err != nil {
		t.Fatal(err)
	}
	m := &fakeMasker{j: e.j}
	e.svc.Maskers = map[string]domain.Masker{"greenmask": m}
	e.j.entries = nil
	return e, m
}

func TestMaskBase_元のベースを一時サーバーで起動し空のベースへマスクして撮る(t *testing.T) {
	e, m := maskEnv(t)
	prod, _ := e.store.GetBase(ctx, name("prod"))
	snap, err := e.svc.MaskBase(ctx, dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Masker: "greenmask", Rules: "users.email: email", By: "ann@x.io"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.MaskInfo{Source: name("prod"), SourceSnapshot: prod.Snapshots[0].Name, Masker: "greenmask", RulesSHA256: sha("users.email: email"), By: "ann@x.io"}
	if snap.Mask == nil || *snap.Mask != want {
		t.Errorf("mask info %+v", snap.Mask)
	}
	dev, _ := e.store.GetBase(ctx, name("dev"))
	if dev.Status != domain.BaseReady || len(dev.Snapshots) != 2 || dev.Snapshots[1].Mask == nil {
		t.Errorf("dev %+v", dev)
	}
	job := m.jobs[0]
	if job.Rules != "users.email: email" || job.Source.Socket != "/run/mask-dev" || job.Target.Socket != "/run/base-dev" || job.Source.Port == job.Target.Port {
		t.Errorf("job %+v", job)
	}
	for _, w := range []string{"zfs clone tank/db-branch/bases/prod@" + prod.Snapshots[0].Name + " tank/db-branch/work/mask-dev", "start mask-dev", "remove mask-dev", "zfs destroy tank/db-branch/work/mask-dev"} {
		if !e.j.has(w) {
			t.Errorf("missing %q in %v", w, e.j.entries)
		}
	}
	if e.j.indexOf("zfs destroy tank/db-branch/work/mask-dev") < e.j.indexOf("mask 40000 -> 40001") && e.j.indexOf("mask 40001 -> 40000") < 0 {
		t.Errorf("order %v", e.j.entries)
	}
}

func TestMaskBase_方針が承認済みのルールに替え確かめて記録する(t *testing.T) {
	e, m := maskEnv(t)
	pol := &fakePolicy{rules: "approved rules"}
	e.svc.MaskPolicy = pol
	snap, err := e.svc.MaskBase(ctx, dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Masker: "greenmask", Rules: "mine"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.jobs[0].Rules != "approved rules" || snap.Mask.RulesSHA256 != sha("approved rules") {
		t.Errorf("rules %q %+v", m.jobs[0].Rules, snap.Mask)
	}
	if len(pol.recorded) != 1 || pol.recorded[0] != "dev@"+snap.Name || pol.plans[0].SourceDB.Socket != "/run/mask-dev" {
		t.Errorf("recorded %v plans %+v", pol.recorded, pol.plans)
	}
}

func TestMaskBase_失敗したらマスク先を前のスナップショットに戻す(t *testing.T) {
	for name_, setup := range map[string]func(*env, *fakeMasker){
		"masker": func(_ *env, m *fakeMasker) { m.err = errors.New("greenmask: exit 1") },
		"verify": func(e *env, _ *fakeMasker) {
			e.svc.MaskPolicy = &fakePolicy{rules: "r", verifyFail: errors.New("emails remain")}
		},
		"refused": func(e *env, _ *fakeMasker) {
			e.svc.MaskPolicy = &fakePolicy{refuse: errors.New("unclassified: users.phone")}
		},
	} {
		t.Run(name_, func(t *testing.T) {
			e, m := maskEnv(t)
			setup(e, m)
			before, _ := e.store.GetBase(ctx, name("dev"))
			if _, err := e.svc.MaskBase(ctx, dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Masker: "greenmask", Rules: "r"}, nil); err == nil {
				t.Fatal("no error")
			}
			dev, _ := e.store.GetBase(ctx, name("dev"))
			if dev.Status != domain.BaseReady || len(dev.Snapshots) != len(before.Snapshots) {
				t.Errorf("dev %+v", dev)
			}
			if name_ != "refused" && !e.j.has("zfs rollback tank/db-branch/bases/dev@"+before.Snapshots[0].Name) {
				t.Errorf("not rolled back: %v", e.j.entries)
			}
			if !e.j.has("zfs destroy tank/db-branch/work/mask-dev") {
				t.Errorf("work copy left: %v", e.j.entries)
			}
		})
	}
}

func TestMaskBase_おかしな依頼は何もせずに断る(t *testing.T) {
	e, _ := maskEnv(t)
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("pg"), Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}}, nil); err != nil {
		e.store.PutBase(ctx, domain.Base{Name: name("pg"), Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}, Status: domain.BaseReady, Node: "node1", Snapshots: []domain.BaseSnapshot{{Name: "s1"}}})
	}
	e.j.entries = nil
	for what, req := range map[string]dbbranch.MaskRequest{
		"unknown masker":   {Target: name("dev"), Source: name("prod"), Masker: "nope"},
		"same base":        {Target: name("prod"), Source: name("prod"), Masker: "greenmask"},
		"other engine":     {Target: name("pg"), Source: name("prod"), Masker: "greenmask"},
		"missing source":   {Target: name("dev"), Source: name("ghost"), Masker: "greenmask"},
		"missing snapshot": {Target: name("dev"), Source: name("prod"), Snapshot: "s0", Masker: "greenmask"},
		"missing target":   {Target: name("ghost"), Source: name("prod"), Masker: "greenmask"},
	} {
		if _, err := e.svc.MaskBase(ctx, req, nil); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	if e.j.has("start mask-dev") {
		t.Errorf("started something: %v", e.j.entries)
	}
	if dev, _ := e.store.GetBase(ctx, name("dev")); dev.Status != domain.BaseReady {
		t.Errorf("dev left %s", dev.Status)
	}
}

// remoteMaskPair: "prod" lives on node1; the target "dev" and the masker on node2.
func remoteMaskPair(t *testing.T) *pair {
	t.Helper()
	p := newPair(t)
	p.withBase(t)
	if _, err := p.replica.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("dev"), Runtime: mysql84}, nil); err != nil {
		t.Fatal(err)
	}
	p.replica.svc.Maskers = map[string]domain.Masker{"greenmask": &fakeMasker{j: p.replica.j}}
	return p
}

func TestMaskBase_元が別のノードにあり新しいスナップショットが未複製なら先に複製する(t *testing.T) {
	p := remoteMaskPair(t)
	s2 := p.snapshot(t)
	p.syncOriginSnaps(t)
	p.replica.j.entries = nil
	if _, err := p.replica.svc.MaskBase(ctx, dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Snapshot: s2, Masker: "greenmask", By: "refresh"}, nil); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has("zfs clone tank/db-branch/replicas/prod@" + s2 + " tank/db-branch/work/mask-dev") {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
}

func TestMaskBase_複製できなければ複製がまだないと断る(t *testing.T) {
	p := remoteMaskPair(t)
	s2 := p.snapshot(t)
	p.syncOriginSnaps(t)
	p.streams.failOn = "recv"
	_, err := p.replica.svc.MaskBase(ctx, dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Snapshot: s2, Masker: "greenmask"}, nil)
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "not replicated") {
		t.Errorf("err = %v", err)
	}
	if dev, _ := p.replica.store.GetBase(ctx, name("dev")); dev.Status != domain.BaseReady {
		t.Errorf("the target was touched: %+v", dev)
	}
}

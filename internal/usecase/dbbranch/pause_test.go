package dbbranch_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestPauseBranch_正常停止してデータを残したまま一時停止にする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "feature-x")
	e.j.entries = nil
	var phases []string

	b, err := e.svc.PauseBranch(ctx, name("feature-x"), func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop branch-feature-x clean"}; !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q (the data and the server's record stay: nothing is removed)", e.j.entries)
	}
	if !reflect.DeepEqual(phases, []string{"stopping"}) {
		t.Errorf("phases = %q", phases)
	}
	got, _ := e.store.GetBranch(ctx, name("feature-x"))
	if b.Status != domain.BranchPaused || got.Status != domain.BranchPaused || got.Error != "" {
		t.Errorf("branch = %+v / stored %+v", b, got)
	}
}

func TestPauseBranch_準備完了でなければ断り_ないブランチはNotFound(t *testing.T) {
	e := newEnv(t)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("busy"), Status: domain.BranchResetting})
	if _, err := e.svc.PauseBranch(ctx, name("busy"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy: %v", err)
	}
	if _, err := e.svc.PauseBranch(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestPauseBranch_止められなければ元の起動状態に戻して失敗を返す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	e.server.failOn = "stop-clean"
	if _, err := e.svc.PauseBranch(ctx, name("x"), nil); err == nil {
		t.Fatal("want an error")
	}
	got, _ := e.store.GetBranch(ctx, name("x"))
	if got.Status != domain.BranchReady {
		t.Errorf("a failed pause leaves the branch ready (running), not failed: %s", got.Status)
	}
}

func TestPauseBranch_全ノードがプロトコル18になるまでは断る(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	n := e.clustered()
	n.nodes[1].Protocol = domain.ProtocolBranchPause - 1
	if _, err := e.svc.PauseBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("an older node: %v", err)
	}
	for i := range n.nodes {
		n.nodes[i].Protocol = domain.ProtocolBranchPause
	}
	if _, err := e.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Errorf("every node at 18: %v", err)
	}
}

func TestResumeBranch_サーバーを起動して準備完了に戻す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "feature-x")
	before, _ := e.store.GetBranch(ctx, name("feature-x"))
	if _, err := e.svc.PauseBranch(ctx, name("feature-x"), nil); err != nil {
		t.Fatal(err)
	}
	e.j.entries = nil
	var phases []string

	b, err := e.svc.ResumeBranch(ctx, name("feature-x"), func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"start branch-feature-x"}; !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q", e.j.entries)
	}
	if !reflect.DeepEqual(phases, []string{"starting"}) {
		t.Errorf("phases = %q", phases)
	}
	if b.Status != domain.BranchReady || b.Port != before.Port || b.Password != before.Password || b.ResetAt != before.ResetAt {
		t.Errorf("the branch keeps its port, password and reset point: %+v", b)
	}
}

func TestResumeBranch_一時停止中でなければ断る(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	if _, err := e.svc.ResumeBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a running branch: %v", err)
	}
	if _, err := e.svc.ResumeBranch(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestResumeBranch_起動できなければ一時停止に戻して理由を残し_やり直せる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	if _, err := e.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	e.server.failOn = "start"
	if _, err := e.svc.ResumeBranch(ctx, name("x"), nil); err == nil {
		t.Fatal("want an error")
	}
	got, _ := e.store.GetBranch(ctx, name("x"))
	if got.Status != domain.BranchPaused || got.Error == "" {
		t.Errorf("it stays paused and says why: %+v", got)
	}
	e.server.failOn = ""
	if b, err := e.svc.ResumeBranch(ctx, name("x"), nil); err != nil || b.Status != domain.BranchReady || b.Error != "" {
		t.Errorf("retry: %+v %v", b, err)
	}
}

func TestRecover_停止の途中は一時停止まで_再開の途中は起動まで進め_一時停止中は起動しない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	for _, tc := range []struct {
		name   string
		status domain.BranchStatus
		want   domain.BranchStatus
		calls  []string
	}{
		{"pausing", domain.BranchPausing, domain.BranchPaused, []string{"stop branch-pausing clean"}},
		{"resuming", domain.BranchResuming, domain.BranchReady, []string{"start branch-resuming"}},
		{"paused", domain.BranchPaused, domain.BranchPaused, nil},
	} {
		e.withBranch(t, tc.name)
		b, _ := e.store.GetBranch(ctx, name(tc.name))
		b.Status = tc.status
		_ = e.store.PutBranch(ctx, b)
	}
	e.j.entries = nil
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want domain.BranchStatus
	}{{"pausing", domain.BranchPaused}, {"resuming", domain.BranchReady}, {"paused", domain.BranchPaused}} {
		if b, _ := e.store.GetBranch(ctx, name(tc.name)); b.Status != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, b.Status, tc.want)
		}
	}
	for _, entry := range e.j.entries {
		if entry == "start branch-paused" {
			t.Errorf("a paused branch is not started: %q", e.j.entries)
		}
	}
}

func TestDeleteBranch_一時停止中のブランチも消せる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	if _, err := e.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	e.j.entries = nil
	if err := e.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
}

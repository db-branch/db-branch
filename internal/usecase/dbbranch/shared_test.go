package dbbranch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// fakeShared is the per-node side of storage every node sees.
type fakeShared struct {
	j       *journal
	mounted []string
	fail    error
}

func (f *fakeShared) Mounted(context.Context, string) ([]string, error) { return f.mounted, f.fail }
func (f *fakeShared) Release(_ context.Context, ds string) error {
	f.j.add("release %s", ds)
	return f.fail
}

// sharedPair is node1 and node2 of a cluster on one shared storage.
func sharedPair(t *testing.T) (o, r *env) {
	t.Helper()
	o, r = newEnv(t), newEnv(t)
	o.clustered()
	r.store, r.svc.Store, r.svc.Node = o.store, o.store, "node2"
	r.clustered()
	r.j, r.storage, r.svc.Storage = o.j, o.storage, o.storage
	o.svc.Shared, r.svc.Shared = &fakeShared{j: o.j}, &fakeShared{j: o.j}
	o.withBase(t)
	return o, r
}

func TestShared_ベースを写さずにどのノードでも元のベースから作る(t *testing.T) {
	o, r := sharedPair(t)
	node, err := r.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node3"})
	if err != nil || node != "node3" {
		t.Fatalf("a node without a copy: %q %v", node, err)
	}
	b, err := r.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"}, nil)
	if err != nil || b.Node != "node2" {
		t.Fatalf("%+v %v", b, err)
	}
	if !o.j.has("zfs clone tank/db-branch/bases/prod@" + b.Snapshot + " tank/db-branch/branches/x") {
		t.Errorf("journal = %v", o.j.entries)
	}
}

func TestShared_失われたブランチは古いボリュームを消してから作り直す(t *testing.T) {
	o, r := sharedPair(t)
	if _, err := o.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node1"}, nil); err != nil {
		t.Fatal(err)
	}
	r.svc.Nodes.(*fakeNodes).nodes[0].LastSeen = r.now.Add(-time.Minute)
	_, _ = r.svc.MarkLost(ctx)
	if node, err := r.svc.PlaceRecreate(ctx, name("x")); err != nil || node == "node1" {
		t.Fatalf("PlaceRecreate = %q %v", node, err)
	}
	o.j.entries = nil
	b, err := r.svc.RecreateBranch(ctx, name("x"), nil)
	if err != nil || b.Node != "node2" || b.Status != domain.BranchReady {
		t.Fatalf("%+v %v", b, err)
	}
	destroy, clone := o.j.indexOf("zfs destroy tank/db-branch/branches/x"), o.j.indexOf("zfs clone tank/db-branch/bases/prod@"+b.Snapshot+" tank/db-branch/branches/x")
	if destroy < 0 || clone < destroy {
		t.Errorf("journal = %v", o.j.entries)
	}
}

func TestShared_戻ったノードは移ったブランチを手放すだけで消さない(t *testing.T) {
	o, r := sharedPair(t)
	for _, n := range []string{"x", "y"} {
		if _, err := o.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod"), Node: "node1"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	o.storage.datasets["tank/db-branch/branches"] = true
	r.svc.Nodes.(*fakeNodes).nodes[0].LastSeen = r.now.Add(-time.Minute)
	_, _ = r.svc.MarkLost(ctx)
	if _, err := r.svc.RecreateBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	// node1 comes back with x and y still mounted.
	o.svc.Shared.(*fakeShared).mounted = []string{"x", "y", "unknown"}
	o.j.entries = nil
	if err := o.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !o.j.has("remove branch-x") || !o.j.has("release tank/db-branch/branches/x") {
		t.Errorf("journal = %v", o.j.entries)
	}
	for _, e := range []string{"zfs destroy tank/db-branch/branches/x", "release tank/db-branch/branches/y", "release tank/db-branch/branches/unknown"} {
		if o.j.has(e) {
			t.Errorf("%s: %v", e, o.j.entries)
		}
	}
	o.svc.Shared.(*fakeShared).fail = errors.New("nfs down")
	if err := o.svc.Reconcile(ctx); err == nil {
		t.Error("a failing mount table")
	}
}

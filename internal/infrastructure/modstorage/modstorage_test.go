package modstorage_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/modstorage"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

type rec struct{ calls []string }

func (r *rec) add(f string, a ...any) error {
	r.calls = append(r.calls, fmt.Sprintf(f, a...))
	return nil
}
func (r *rec) Create(_ context.Context, ds string, _ map[string]string) error {
	return r.add("create %s", ds)
}
func (r *rec) Snapshot(_ context.Context, ds, s string) error { return r.add("snapshot %s@%s", ds, s) }
func (r *rec) Clone(_ context.Context, ds, s, to string) error {
	return r.add("clone %s@%s %s", ds, s, to)
}
func (r *rec) Rollback(_ context.Context, ds, s string) error { return r.add("rollback %s@%s", ds, s) }
func (r *rec) Destroy(_ context.Context, ds string) error     { return r.add("destroy %s", ds) }
func (r *rec) DestroySnapshot(_ context.Context, ds, s string) error {
	return r.add("destroy %s@%s", ds, s)
}
func (r *rec) Exists(_ context.Context, ds string) (bool, error) { return true, r.add("exists %s", ds) }
func (r *rec) Info(_ context.Context, ds string) (ext.DatasetInfo, error) {
	return ext.DatasetInfo{Mountpoint: "/m/" + ds, Used: 3, Referenced: 9}, r.add("info %s", ds)
}
func (r *rec) Mounted(_ context.Context, p string) ([]string, error) {
	return []string{"a"}, r.add("mounted %s", p)
}
func (r *rec) Release(_ context.Context, ds string) error { return r.add("release %s", ds) }

func TestStorage_モジュールのストレージをノードのストレージとして使う(t *testing.T) {
	r := &rec{}
	s := &modstorage.Storage{Module: "fsx"}
	s.Attach(r)
	var _ domain.Storage = s
	var _ domain.SharedStorage = s
	snap := domain.Snapshot{Dataset: "fsx/db-branch/bases/prod", Name: "s1"}
	_ = s.Create(ctx, "fsx/db-branch", nil)
	_ = s.Snapshot(ctx, snap)
	_ = s.Clone(ctx, snap, "fsx/db-branch/branches/x")
	_ = s.Rollback(ctx, snap)
	_ = s.DestroySnapshot(ctx, snap)
	_ = s.Destroy(ctx, "fsx/db-branch/branches/x")
	_ = s.Release(ctx, "fsx/db-branch/branches/x")
	ok, _ := s.Exists(ctx, "fsx/db-branch")
	in, _ := s.Info(ctx, "fsx/db-branch/bases/prod")
	names, _ := s.Mounted(ctx, "fsx/db-branch/branches")
	if !ok || in.Mountpoint != "/m/fsx/db-branch/bases/prod" || in.Used != 3 || in.Referenced != 9 || len(names) != 1 {
		t.Errorf("%v %+v %v", ok, in, names)
	}
	if got := strings.Join(r.calls, "|"); !strings.HasPrefix(got, "create fsx/db-branch|snapshot fsx/db-branch/bases/prod@s1|clone fsx/db-branch/bases/prod@s1 fsx/db-branch/branches/x|rollback") {
		t.Errorf("calls %s", got)
	}
}

func TestStorage_モジュールが読み込まれるまでは使えないと伝える(t *testing.T) {
	s := &modstorage.Storage{Module: "fsx"}
	snap := domain.Snapshot{Dataset: "a", Name: "b"}
	errs := []error{s.Create(ctx, "a", nil), s.Snapshot(ctx, snap), s.Clone(ctx, snap, "c"), s.Rollback(ctx, snap), s.Destroy(ctx, "a"), s.DestroySnapshot(ctx, snap), s.Release(ctx, "a")}
	_, e1 := s.Exists(ctx, "a")
	_, e2 := s.Info(ctx, "a")
	_, e3 := s.Mounted(ctx, "a")
	for i, err := range append(errs, e1, e2, e3) {
		if !errors.Is(err, modstorage.ErrNotLoaded) || !strings.Contains(err.Error(), "fsx") {
			t.Errorf("%d: %v", i, err)
		}
	}
}

package zfs_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
)

var _ domain.CheckpointStorage = (*zfs.Driver)(nil)

func TestDatasets_直下の子を完全名で返し自分は含めない(t *testing.T) {
	f := &fakeRunner{out: []byte("tank/h/x\ntank/h/x/1\ntank/h/x/new\n")}
	got, err := zfs.New(f).Datasets(ctx, "tank/h/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tank/h/x/1", "tank/h/x/new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	assertCalls(t, f, call{"zfs", "list", "-H", "-d", "1", "-t", "filesystem", "-o", "name", "tank/h/x"})
	if _, err := zfs.New(f).Datasets(ctx, "-bad"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad parent: %v", err)
	}
}

const snapshotsOut = "" +
	"tank/b/x@reset\tcreatetxg\t10\t-\n" +
	"tank/b/x@reset\tcreation\t1794480000\t-\n" +
	"tank/b/x@reset\tused\t0\t-\n" +
	"tank/b/x@reset\tclones\t-\t-\n" +
	"tank/b/x@reset\tdb-branch:line\t3\tinherited from tank/b/x\n" +
	"tank/b/x@ckpt-c1\tcreatetxg\t20\t-\n" +
	"tank/b/x@ckpt-c1\tcreation\t1794480060\t-\n" +
	"tank/b/x@ckpt-c1\tused\t4096\t-\n" +
	"tank/b/x@ckpt-c1\tclones\ttank/h/x/1,tank/h/x/2\t-\n" +
	"tank/b/x@ckpt-c1\tdb-branch:line\t1\tlocal\n" +
	"tank/b/x@ckpt-c1\tdb-branch:auto\toff\tlocal\n"

func TestSnapshotInfos_作成順に時刻と容量とクローンと自前のプロパティを返す(t *testing.T) {
	f := &fakeRunner{out: []byte(snapshotsOut)}
	got, err := zfs.New(f).SnapshotInfos(ctx, "tank/b/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "reset" || got[1].Name != "ckpt-c1" {
		t.Fatalf("got %+v", got)
	}
	if len(got[0].Props) != 0 || len(got[0].Clones) != 0 {
		t.Errorf("inherited properties must not count: %+v", got[0])
	}
	c := got[1]
	if !c.Created.Equal(time.Unix(1794480060, 0)) || c.Used != 4096 || !reflect.DeepEqual(c.Clones, []string{"tank/h/x/1", "tank/h/x/2"}) ||
		c.Props[domain.PropLine] != "1" || c.Props[domain.PropAuto] != "off" {
		t.Errorf("checkpoint = %+v", c)
	}
	assertCalls(t, f, call{"zfs", "get", "-Hp", "-d", "1", "-t", "snapshot", "-o", "name,property,value,source",
		"createtxg,creation,used,clones,db-branch:line,db-branch:auto", "tank/b/x"})
}

func TestSnapshotInfos_壊れた出力と不正な名前はエラー(t *testing.T) {
	for _, out := range []string{"tank/b/x@a\tused\tmany\t-\n", "tank/b/x@a\tcreation\tsoon\t-\n", "tank/b/x@a\tcreatetxg\tx\t-\n", "garbage\n"} {
		if _, err := zfs.New(&fakeRunner{out: []byte(out)}).SnapshotInfos(ctx, "tank/b/x"); err == nil {
			t.Errorf("%q: want error", out)
		}
	}
	if _, err := zfs.New(&fakeRunner{}).Snapshots(ctx, "a b"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad dataset: %v", err)
	}
	if _, err := zfs.New(&fakeRunner{err: errors.New("boom")}).SnapshotInfos(ctx, "tank/b/x"); err == nil {
		t.Error("runner error must surface")
	}
}

func TestProps_自分で設定したユーザープロパティだけ返す(t *testing.T) {
	f := &fakeRunner{out: []byte("mountpoint\t/x\ndb-branch:line\t2\ndb-branch:head\tc1\nrecordsize\t16K\n")}
	got, err := zfs.New(f).Props(ctx, "tank/b/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"db-branch:line": "2", "db-branch:head": "c1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	assertCalls(t, f, call{"zfs", "get", "-H", "-s", "local", "-o", "property,value", "all", "tank/b/x"})
	if _, err := zfs.New(&fakeRunner{err: errors.New("x")}).Props(ctx, "tank/b/x"); err == nil {
		t.Error("runner error must surface")
	}
	if _, err := zfs.New(f).Props(ctx, "-x"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad dataset: %v", err)
	}
}

func TestSetProps_データセットにもスナップショットにも一度に設定する(t *testing.T) {
	f := &fakeRunner{}
	d := zfs.New(f)
	if err := d.SetProps(ctx, "tank/b/x@ckpt-c1", map[string]string{"db-branch:line": "1", "db-branch:auto": "off"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProps(ctx, "tank/b/x", map[string]string{"canmount": "off"}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f,
		call{"zfs", "set", "db-branch:auto=off", "db-branch:line=1", "tank/b/x@ckpt-c1"},
		call{"zfs", "set", "canmount=off", "tank/b/x"})
}

func TestSetProps_不正な指定は渡さない(t *testing.T) {
	f := &fakeRunner{}
	d := zfs.New(f)
	for _, c := range []struct {
		target string
		props  map[string]string
	}{
		{"tank/b/x@a b", map[string]string{"a": "b"}},
		{"-x", map[string]string{"a": "b"}},
		{"tank/b/x", map[string]string{"Bad Key": "b"}},
		{"tank/b/x", map[string]string{"a": "b c"}},
		{"tank/b/x", nil},
	} {
		if err := d.SetProps(ctx, c.target, c.props); !errors.Is(err, zfs.ErrInvalidDataset) {
			t.Errorf("%v: %v", c, err)
		}
	}
	assertCalls(t, f)
}

func TestUnset_ユーザープロパティを継承に戻して消す(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).Unset(ctx, "tank/h/x", "db-branch:op", "db-branch:source"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "inherit", "db-branch:op", "tank/h/x"}, call{"zfs", "inherit", "db-branch:source", "tank/h/x"})
	if err := zfs.New(f).Unset(ctx, "tank/h/x", "Bad"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad key: %v", err)
	}
	if err := zfs.New(&fakeRunner{err: errors.New("x")}).Unset(ctx, "tank/h/x", "db-branch:op"); err == nil {
		t.Error("runner error must surface")
	}
}

func TestCloneWith_プロパティを指定してクローンを作る(t *testing.T) {
	f := &fakeRunner{}
	err := zfs.New(f).CloneWith(ctx, domain.Snapshot{Dataset: "tank/b/x", Name: "ckpt-c1"}, "tank/h/x/new",
		map[string]string{"canmount": "off", "db-branch:line": "2"})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "clone", "-o", "canmount=off", "-o", "db-branch:line=2", "tank/b/x@ckpt-c1", "tank/h/x/new"})
	d := zfs.New(f)
	if err := d.CloneWith(ctx, domain.Snapshot{Dataset: "tank/b/x", Name: "a b"}, "tank/h/x/new", nil); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad origin: %v", err)
	}
	if err := d.CloneWith(ctx, domain.Snapshot{Dataset: "tank/b/x", Name: "a"}, "-x", nil); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad target: %v", err)
	}
	if err := d.CloneWith(ctx, domain.Snapshot{Dataset: "tank/b/x", Name: "a"}, "tank/y", map[string]string{"Bad": "x"}); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad prop: %v", err)
	}
}

func TestRenameとPromoteとRenameSnapshot(t *testing.T) {
	f := &fakeRunner{}
	d := zfs.New(f)
	if err := d.Rename(ctx, "tank/b/x", "tank/h/x/0"); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameSnapshot(ctx, domain.Snapshot{Dataset: "tank/b/x", Name: "ckpt-a"}, "gone-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Promote(ctx, "tank/b/x"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "rename", "-p", "tank/b/x", "tank/h/x/0"}, call{"zfs", "rename", "tank/b/x@ckpt-a", "tank/b/x@gone-1"}, call{"zfs", "promote", "tank/b/x"})
	for _, err := range []error{
		d.Rename(ctx, "tank", "tank/x"), d.Rename(ctx, "tank/x", "tank"), d.Rename(ctx, "-x", "tank/y"), d.Rename(ctx, "tank/y", "a b"),
		d.RenameSnapshot(ctx, domain.Snapshot{Dataset: "tank/x", Name: "a"}, "b c"), d.RenameSnapshot(ctx, domain.Snapshot{Dataset: "-x", Name: "a"}, "b"),
		d.Promote(ctx, "-x"), d.Promote(ctx, "tank"),
	} {
		if !errors.Is(err, zfs.ErrInvalidDataset) {
			t.Errorf("error = %v, want ErrInvalidDataset", err)
		}
	}
}

func TestOrigin_元のスナップショットを返しなければ空(t *testing.T) {
	d := zfs.New(&fakeRunner{out: []byte("tank/h/x/0@ckpt-c1\n")})
	if got, err := d.Origin(ctx, "tank/b/x"); err != nil || got != "tank/h/x/0@ckpt-c1" {
		t.Errorf("%q %v", got, err)
	}
	f := &fakeRunner{out: []byte("-\n")}
	if got, err := zfs.New(f).Origin(ctx, "tank/b/x"); err != nil || got != "" {
		t.Errorf("%q %v", got, err)
	}
	assertCalls(t, f, call{"zfs", "get", "-H", "-o", "value", "origin", "tank/b/x"})
	if _, err := zfs.New(&fakeRunner{err: errors.New("x")}).Origin(ctx, "tank/b/x"); err == nil {
		t.Error("runner error must surface")
	}
	if _, err := d.Origin(ctx, "-x"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad dataset: %v", err)
	}
}

func TestMount_マウント済みは成功とみなす(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).Mount(ctx, "tank/b/x"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "mount", "tank/b/x"})
	already := &fakeRunner{err: &execx.Error{Stderr: "cannot mount 'tank/b/x': filesystem already mounted", Err: errors.New("exit 1")}}
	if err := zfs.New(already).Mount(ctx, "tank/b/x"); err != nil {
		t.Errorf("already mounted: %v", err)
	}
	if err := zfs.New(&fakeRunner{err: errors.New("busy")}).Mount(ctx, "tank/b/x"); err == nil {
		t.Error("other failures must surface")
	}
	if err := zfs.New(f).Mount(ctx, "-x"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad dataset: %v", err)
	}
}

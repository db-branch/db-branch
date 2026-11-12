package zfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
)

type call []string

type fakeRunner struct {
	calls []call
	out   []byte
	err   error
	stdin []byte
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append(call{name}, args...))
	return f.out, f.err
}

func (f *fakeRunner) Stream(_ context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, append(call{name}, args...))
	if in != nil {
		f.stdin, _ = io.ReadAll(in)
	}
	_, _ = out.Write(f.out)
	return f.err
}

func assertCalls(t *testing.T, f *fakeRunner, want ...call) {
	t.Helper()
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

var ctx = context.Background()

func TestSnapshot_スナップショットを作る(t *testing.T) {
	f := &fakeRunner{}
	err := zfs.New(f).Snapshot(ctx, domain.Snapshot{Dataset: "tank/branches/x", Name: "reset"})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "snapshot", "tank/branches/x@reset"})
}

func TestClone_親データセットがなくてもクローンを作る(t *testing.T) {
	f := &fakeRunner{}
	err := zfs.New(f).Clone(ctx, domain.Snapshot{Dataset: "tank/bases/prod", Name: "s1"}, "tank/branches/x")
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "clone", "-p", "tank/bases/prod@s1", "tank/branches/x"})
}

func TestRollback_スナップショットの時点に戻す(t *testing.T) {
	f := &fakeRunner{}
	err := zfs.New(f).Rollback(ctx, domain.Snapshot{Dataset: "tank/branches/x", Name: "reset"})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "rollback", "tank/branches/x@reset"})
}

func TestDestroy_データセットをスナップショットごと消す(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).Destroy(ctx, "tank/branches/x"); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "destroy", "-r", "tank/branches/x"})
}

func TestDestroySnapshot_スナップショットだけを消す(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).DestroySnapshot(ctx, domain.Snapshot{Dataset: "tank/bases/x", Name: "s1"}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "destroy", "tank/bases/x@s1"})
	if err := zfs.New(f).DestroySnapshot(ctx, domain.Snapshot{Dataset: "tank/bases/x", Name: "a b"}); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("bad name: %v", err)
	}
}

func TestDestroy_プールそのものは消さない(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).Destroy(ctx, "tank"); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Fatalf("error = %v, want ErrInvalidDataset", err)
	}
	assertCalls(t, f)
}

func TestExists_あればtrueを返す(t *testing.T) {
	f := &fakeRunner{out: []byte("tank/branches/x\n")}
	ok, err := zfs.New(f).Exists(ctx, "tank/branches/x")
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true, nil", ok, err)
	}
	assertCalls(t, f, call{"zfs", "list", "-H", "-o", "name", "tank/branches/x"})
}

func TestExists_なければfalseを返す(t *testing.T) {
	f := &fakeRunner{err: &execx.Error{Stderr: "cannot open 'tank/branches/x': dataset does not exist", Err: errors.New("exit 1")}}
	ok, err := zfs.New(f).Exists(ctx, "tank/branches/x")
	if err != nil || ok {
		t.Fatalf("Exists = %v, %v; want false, nil", ok, err)
	}
}

func TestExists_それ以外の失敗はエラーにする(t *testing.T) {
	f := &fakeRunner{err: &execx.Error{Stderr: "permission denied", Err: errors.New("exit 1")}}
	if _, err := zfs.New(f).Exists(ctx, "tank/branches/x"); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestInfo_マウント先と容量を返す(t *testing.T) {
	f := &fakeRunner{out: []byte("/tank/branches/x\n1048576\n524288000\n")}
	got, err := zfs.New(f).Info(ctx, "tank/branches/x")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.DatasetInfo{Mountpoint: "/tank/branches/x", Used: 1048576, Referenced: 524288000}
	if got != want {
		t.Errorf("Info = %+v, want %+v", got, want)
	}
	assertCalls(t, f, call{"zfs", "get", "-Hp", "-o", "value", "mountpoint,used,referenced", "tank/branches/x"})
}

func TestInfo_想定外の出力はエラーにする(t *testing.T) {
	for _, out := range []string{"", "/m\n1\n", "/m\nx\n2\n", "/m\n1\ny\n"} {
		f := &fakeRunner{out: []byte(out)}
		if _, err := zfs.New(f).Info(ctx, "tank/branches/x"); err == nil {
			t.Errorf("output %q: want error, got nil", out)
		}
	}
}

func TestZFSコマンドの失敗はエラーとして返す(t *testing.T) {
	boom := errors.New("boom")
	s := domain.Snapshot{Dataset: "tank/a", Name: "s"}
	ops := map[string]func(*zfs.Driver) error{
		"snapshot": func(d *zfs.Driver) error { return d.Snapshot(ctx, s) },
		"clone":    func(d *zfs.Driver) error { return d.Clone(ctx, s, "tank/b") },
		"rollback": func(d *zfs.Driver) error { return d.Rollback(ctx, s) },
		"destroy":  func(d *zfs.Driver) error { return d.Destroy(ctx, "tank/a") },
		"info":     func(d *zfs.Driver) error { _, err := d.Info(ctx, "tank/a"); return err },
	}
	for name, op := range ops {
		if err := op(zfs.New(&fakeRunner{err: boom})); !errors.Is(err, boom) {
			t.Errorf("%s: error = %v, want wrapping boom", name, err)
		}
	}
}

func Test不正な名前はzfsに渡さない(t *testing.T) {
	bad := []string{"", "-r", "tank/-o", "tank//a", "tank/a/", "/tank", "tank/a b", "tank/a@s", "tank/a;rm", "tank/a\n"}
	for _, ds := range bad {
		f := &fakeRunner{}
		d := zfs.New(f)
		errs := []error{
			d.Snapshot(ctx, domain.Snapshot{Dataset: ds, Name: "s"}),
			d.Clone(ctx, domain.Snapshot{Dataset: "tank/a", Name: "s"}, ds),
			d.Clone(ctx, domain.Snapshot{Dataset: ds, Name: "s"}, "tank/b"),
			d.Rollback(ctx, domain.Snapshot{Dataset: ds, Name: "s"}),
			d.Destroy(ctx, ds),
			func() error { _, err := d.Exists(ctx, ds); return err }(),
			func() error { _, err := d.Info(ctx, ds); return err }(),
		}
		for i, err := range errs {
			if !errors.Is(err, zfs.ErrInvalidDataset) {
				t.Errorf("dataset %q op %d: error = %v, want ErrInvalidDataset", ds, i, err)
			}
		}
		if len(f.calls) != 0 {
			t.Errorf("dataset %q: zfs was called: %q", ds, f.calls)
		}
	}
	for _, snap := range []string{"", "-r", "a b", "a@b", "a/b"} {
		f := &fakeRunner{}
		err := zfs.New(f).Snapshot(ctx, domain.Snapshot{Dataset: "tank/a", Name: snap})
		if !errors.Is(err, zfs.ErrInvalidDataset) || len(f.calls) != 0 {
			t.Errorf("snapshot name %q: error = %v, calls = %q", snap, err, f.calls)
		}
	}
}

func TestCreate_親ごとデータセットを作りプロパティを設定する(t *testing.T) {
	f := &fakeRunner{}
	err := zfs.New(f).Create(ctx, "tank/db-branch/bases/prod", map[string]string{"recordsize": "16K", "compression": "lz4"})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "create", "-p", "-o", "compression=lz4", "-o", "recordsize=16K", "tank/db-branch/bases/prod"})
}

func TestCreate_不正な名前やプロパティは渡さない(t *testing.T) {
	f := &fakeRunner{}
	d := zfs.New(f)
	if err := d.Create(ctx, "tank/-o", nil); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("dataset: %v", err)
	}
	if err := d.Create(ctx, "tank/a", map[string]string{"-o x": "1"}); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("property: %v", err)
	}
	if err := d.Create(ctx, "tank/a", map[string]string{"recordsize": "16K 2"}); !errors.Is(err, zfs.ErrInvalidDataset) {
		t.Errorf("value: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %q", f.calls)
	}
	if err := zfs.New(&fakeRunner{err: errors.New("boom")}).Create(ctx, "tank/a", nil); err == nil {
		t.Error("runner failure: want error")
	}
}

func TestUsage_子データセットごとの使用量を返す(t *testing.T) {
	f := &fakeRunner{out: []byte("tank/db-branch/branches\t300\t100\ntank/db-branch/branches/a\t10\t500\ntank/db-branch/branches/b\t20\t600\n")}
	got, err := zfs.New(f).Usage(ctx, "tank/db-branch/branches")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"] != (domain.Usage{Used: 10, Referenced: 500}) || got["b"].Referenced != 600 {
		t.Errorf("Usage = %+v", got)
	}
	assertCalls(t, f, call{"zfs", "list", "-Hp", "-d", "1", "-o", "name,used,referenced", "tank/db-branch/branches"})
}

func TestUsage_不正な入力や出力はエラー(t *testing.T) {
	if _, err := zfs.New(&fakeRunner{}).Usage(ctx, "-bad"); err == nil {
		t.Error("bad dataset: want error")
	}
	if _, err := zfs.New(&fakeRunner{err: errors.New("boom")}).Usage(ctx, "tank/x"); err == nil {
		t.Error("zfs failure: want error")
	}
	if _, err := zfs.New(&fakeRunner{out: []byte("tank/x/a\tlots\t1\n")}).Usage(ctx, "tank/x"); err == nil {
		t.Error("garbage: want error")
	}
}

func TestSend_初回は全体を差分は間のスナップショットをまとめて送る(t *testing.T) {
	f := &fakeRunner{out: []byte("STREAM")}
	var buf bytes.Buffer
	if err := zfs.New(f).Send(ctx, "tank/db-branch/bases/shop", "", "s2", &buf); err != nil {
		t.Fatal(err)
	}
	if err := zfs.New(f).Send(ctx, "tank/db-branch/bases/shop", "s1", "s3", &buf); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f,
		call{"zfs", "send", "-R", "tank/db-branch/bases/shop@s2"},
		call{"zfs", "send", "-I", "@s1", "tank/db-branch/bases/shop@s3"})
	if buf.String() != "STREAMSTREAM" {
		t.Errorf("stream = %q", buf.String())
	}
}

func TestReceive_マウントせず受け取る(t *testing.T) {
	f := &fakeRunner{}
	if err := zfs.New(f).Receive(ctx, "tank/db-branch/replicas/shop", strings.NewReader("STREAM")); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, f, call{"zfs", "recv", "-u", "-F", "tank/db-branch/replicas/shop"})
	if string(f.stdin) != "STREAM" {
		t.Errorf("stdin = %q", f.stdin)
	}
}

func TestChildren_子の名前を並べて返す(t *testing.T) {
	f := &fakeRunner{out: []byte("tank/r\t1\t1\ntank/r/b\t1\t1\ntank/r/a\t1\t1\n")}
	got, err := zfs.New(f).Children(ctx, "tank/r")
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Errorf("Children = %v %v", got, err)
	}
	if _, err := zfs.New(&fakeRunner{err: errors.New("x")}).Children(ctx, "tank/r"); err == nil {
		t.Error("want error")
	}
}

func TestSendとReceive_不正な名前や失敗はエラー(t *testing.T) {
	d := zfs.New(&fakeRunner{})
	var buf bytes.Buffer
	for name, err := range map[string]error{
		"send bad to":   d.Send(ctx, "tank/x", "", "-bad", &buf),
		"send bad from": d.Send(ctx, "tank/x", "-bad", "s1", &buf),
		"recv bad":      d.Receive(ctx, "-bad", strings.NewReader("")),
	} {
		if !errors.Is(err, zfs.ErrInvalidDataset) {
			t.Errorf("%s: %v", name, err)
		}
	}
	boom := zfs.New(&fakeRunner{err: errors.New("boom")})
	if err := boom.Send(ctx, "tank/x", "", "s1", &buf); err == nil {
		t.Error("send failure: want error")
	}
	if err := boom.Receive(ctx, "tank/x", strings.NewReader("")); err == nil {
		t.Error("recv failure: want error")
	}
}

func TestSnapshots_古い順にスナップショット名を返す(t *testing.T) {
	f := &fakeRunner{out: []byte("tank/r/shop@s1\ntank/r/shop@s2\n")}
	got, err := zfs.New(f).Snapshots(ctx, "tank/r/shop")
	if err != nil || strings.Join(got, ",") != "s1,s2" {
		t.Errorf("Snapshots = %v %v", got, err)
	}
	assertCalls(t, f, call{"zfs", "list", "-H", "-t", "snapshot", "-s", "createtxg", "-o", "name", "-d", "1", "tank/r/shop"})
	if _, err := zfs.New(&fakeRunner{}).Snapshots(ctx, "-x"); err == nil {
		t.Error("bad name: want error")
	}
	if _, err := zfs.New(&fakeRunner{err: errors.New("x")}).Snapshots(ctx, "tank/r"); err == nil {
		t.Error("failure: want error")
	}
	if got, _ := zfs.New(&fakeRunner{}).Snapshots(ctx, "tank/r"); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
}

package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

type optionsModule struct{}

func (optionsModule) ServerOptions(_ context.Context, b ext.ServerBranch) (ext.ServerOptions, error) {
	if b.Name == "nolicence" {
		return ext.ServerOptions{}, ext.ErrForbidden
	}
	return ext.ServerOptions{Config: []string{"log_bin=binlog", "maint=" + map[bool]string{true: "y", false: "n"}[b.Maintenance]}}, nil
}

func (optionsModule) AfterRestore(_ context.Context, b ext.ServerBranch, note string) error {
	if note == "fail" {
		return errors.New("replay failed")
	}
	return nil
}

func Test_サーバーの追加設定と復元後の処理を運ぶ(t *testing.T) {
	r := remoteFor(t, ext.Module{Name: "h", ServerOptioner: optionsModule{}, RestoreFinisher: optionsModule{}})
	info, _ := r.Info()
	if !info.ServerOptioner || !info.RestoreFinisher {
		t.Fatalf("info %+v", info)
	}
	o, err := r.ServerOptions(ctx, ext.ServerBranch{Name: "x", Maintenance: true})
	if err != nil || len(o.Config) != 2 || o.Config[1] != "maint=y" {
		t.Errorf("options %v %v", o, err)
	}
	if _, err := r.ServerOptions(ctx, ext.ServerBranch{Name: "nolicence"}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("a refusal keeps its sentinel: %v", err)
	}
	if err := r.AfterRestore(ctx, ext.ServerBranch{Name: "x"}, "ok"); err != nil {
		t.Error(err)
	}
	if err := r.AfterRestore(ctx, ext.ServerBranch{Name: "x"}, "fail"); err == nil || err.Error() != "replay failed" {
		t.Errorf("failure: %v", err)
	}
	none := remoteFor(t, ext.Module{Name: "none"})
	if o, err := none.ServerOptions(ctx, ext.ServerBranch{}); err != nil || len(o.Config) != 0 {
		t.Errorf("no optioner: %v %v", o, err)
	}
	if err := none.AfterRestore(ctx, ext.ServerBranch{}, "n"); err != nil {
		t.Errorf("no finisher: %v", err)
	}
	_ = r.c.Close()
	if _, err := r.ServerOptions(ctx, ext.ServerBranch{}); err == nil {
		t.Error("closed options")
	}
	if err := r.AfterRestore(ctx, ext.ServerBranch{}, ""); err == nil {
		t.Error("closed after-restore")
	}
}

// fakeHost records the calls a module makes and answers from fixed data.
type fakeHost struct {
	calls []string
	fail  string
}

func (f *fakeHost) rec(s string) error {
	f.calls = append(f.calls, s)
	if f.fail == s {
		return ext.ErrForbidden
	}
	return nil
}

func (f *fakeHost) Branch(_ context.Context, n string) (ext.BranchState, error) {
	return ext.BranchState{Name: n, Owner: "alice", Engine: "mysql", Record: true, CreatedAt: time.Unix(5, 0).UTC()}, f.rec("branch " + n)
}
func (f *fakeHost) Branches(context.Context) ([]ext.BranchState, error) {
	return []ext.BranchState{{Name: "a"}, {Name: "b"}}, f.rec("branches")
}
func (f *fakeHost) Graph(_ context.Context, n string) (ext.CheckpointGraph, error) {
	return ext.CheckpointGraph{Live: 1, Head: "c1", Lines: []ext.LineInfo{{Line: 0, Parent: ext.NoLine}, {Line: 1, Live: true, Parent: 0, From: "c1"}},
		Checkpoints: []ext.CheckpointInfo{{Name: "c1"}}}, f.rec("graph " + n)
}
func (f *fakeHost) CreateCheckpoint(_ context.Context, n, c string) (ext.CheckpointInfo, error) {
	return ext.CheckpointInfo{Name: c}, f.rec("checkpoint " + n + " " + c)
}
func (f *fakeHost) RestoreCheckpoint(_ context.Context, n, c, note string) error {
	return f.rec("restore " + n + " " + c + " " + note)
}
func (f *fakeHost) Reset(_ context.Context, n, note string) error {
	return f.rec("reset " + n + " " + note)
}
func (f *fakeHost) DB(_ context.Context, n string) (ext.DBAccess, error) {
	return ext.DBAccess{Engine: "mysql", Socket: "/run/" + n}, f.rec("db " + n)
}
func (f *fakeHost) OpenLine(_ context.Context, n string, line int, c string) (string, error) {
	return "/mnt/" + n, f.rec("open " + n + " " + c)
}
func (f *fakeHost) CloseLine(_ context.Context, d string) error { return f.rec("close " + d) }
func (f *fakeHost) StartOperation(_ context.Context, k, b string) (string, error) {
	return "op_1", f.rec("start " + k + " " + b)
}
func (f *fakeHost) ReportOperation(_ context.Context, id, p string) error {
	return f.rec("report " + id + " " + p)
}
func (f *fakeHost) EndOperation(_ context.Context, id, fail string) error {
	return f.rec("end " + id + " " + fail)
}

func Test_モジュールはエージェントのブランチ操作を呼べる(t *testing.T) {
	fh := &fakeHost{}
	st := starter{host: make(chan ext.Host, 1)}
	r := remoteFor(t, ext.Module{Name: "m", Starter: st})
	if err := r.Start(ctx, ext.Host{Node: "n1", Store: memStore{}, Branches: fh}); err != nil {
		t.Fatal(err)
	}
	h := (<-st.host).Branches
	if h == nil {
		t.Fatal("no branch host")
	}
	if b, err := h.Branch(ctx, "x"); err != nil || b.Owner != "alice" || !b.Record || !b.CreatedAt.Equal(time.Unix(5, 0)) {
		t.Errorf("branch %+v %v", b, err)
	}
	if l, err := h.Branches(ctx); err != nil || len(l) != 2 {
		t.Errorf("branches %v %v", l, err)
	}
	if g, err := h.Graph(ctx, "x"); err != nil || g.Live != 1 || g.Lines[1].Parent != 0 || g.Lines[0].Parent != ext.NoLine || g.Lines[1].From != "c1" {
		t.Errorf("graph %+v %v", g, err)
	}
	if c, err := h.CreateCheckpoint(ctx, "x", "c9"); err != nil || c.Name != "c9" {
		t.Errorf("checkpoint %+v %v", c, err)
	}
	if err := h.RestoreCheckpoint(ctx, "x", "c1", "note"); err != nil {
		t.Error(err)
	}
	if db, err := h.DB(ctx, "x"); err != nil || db.Socket != "/run/x" {
		t.Errorf("db %+v %v", db, err)
	}
	if d, err := h.OpenLine(ctx, "x", 2, "c1"); err != nil || d != "/mnt/x" {
		t.Errorf("open %v %v", d, err)
	}
	if err := h.CloseLine(ctx, "/mnt/x"); err != nil {
		t.Error(err)
	}
	id, err := h.StartOperation(ctx, "history.restore", "x")
	if err != nil || id != "op_1" {
		t.Fatalf("start %v %v", id, err)
	}
	if err := h.ReportOperation(ctx, id, "replaying"); err != nil {
		t.Error(err)
	}
	if err := h.EndOperation(ctx, id, ""); err != nil {
		t.Error(err)
	}
	want := []string{"branch x", "branches", "graph x", "checkpoint x c9", "restore x c1 note", "db x", "open x c1", "close /mnt/x",
		"start history.restore x", "report op_1 replaying", "end op_1 "}
	if len(fh.calls) != len(want) {
		t.Fatalf("calls = %q", fh.calls)
	}
	for i := range want {
		if fh.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, fh.calls[i], want[i])
		}
	}
	// Failures arrive with their sentinel.
	for _, op := range []string{"branch x", "branches", "graph x", "checkpoint x c9", "restore x c1 note", "db x", "open x c1", "close /mnt/x",
		"start history.restore x", "report op_1 p", "end op_1 boom"} {
		fh.fail = op
		var err error
		switch op {
		case "branch x":
			_, err = h.Branch(ctx, "x")
		case "branches":
			_, err = h.Branches(ctx)
		case "graph x":
			_, err = h.Graph(ctx, "x")
		case "checkpoint x c9":
			_, err = h.CreateCheckpoint(ctx, "x", "c9")
		case "restore x c1 note":
			err = h.RestoreCheckpoint(ctx, "x", "c1", "note")
		case "db x":
			_, err = h.DB(ctx, "x")
		case "open x c1":
			_, err = h.OpenLine(ctx, "x", 0, "c1")
		case "close /mnt/x":
			err = h.CloseLine(ctx, "/mnt/x")
		case "start history.restore x":
			_, err = h.StartOperation(ctx, "history.restore", "x")
		case "report op_1 p":
			err = h.ReportOperation(ctx, "op_1", "p")
		case "end op_1 boom":
			err = h.EndOperation(ctx, "op_1", "boom")
		}
		if !errors.Is(err, ext.ErrForbidden) {
			t.Errorf("%s: err = %v", op, err)
		}
	}
	// An unknown operation and a closed connection are errors too.
	if out, _ := (&BranchHostServer{H: fh}).call(HostCall{Op: "nope"}); out.Err.Msg == "" {
		t.Error("unknown operation: want error")
	}
	h.(*hostRemote).c.Close()
	if _, err := h.Branch(ctx, "x"); err == nil {
		t.Error("closed: want error")
	}
	// A module started by an agent without a branch host gets none.
	bare := starter{host: make(chan ext.Host, 1)}
	r2 := remoteFor(t, ext.Module{Name: "m2", Starter: bare})
	if err := r2.Start(ctx, ext.Host{Store: memStore{}}); err != nil {
		t.Fatal(err)
	}
	if (<-bare.host).Branches != nil {
		t.Error("the host should have no branch host")
	}
}

func (s *BranchHostServer) call(c HostCall) (HostReply, error) {
	var out HostReply
	err := s.Call(c, &out)
	return out, err
}

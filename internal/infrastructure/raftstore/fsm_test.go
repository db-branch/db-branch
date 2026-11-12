package raftstore_test

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

func name(s string) domain.Name { n, _ := domain.ParseName(s); return n }

func apply(t *testing.T, f *raftstore.FSM, c raftstore.Command) {
	t.Helper()
	data, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if res := f.Apply(&raft.Log{Data: data}); res != nil {
		if err, ok := res.(error); ok {
			t.Fatalf("apply %s: %v", c.Op, err)
		}
	}
}

func TestFSM_コマンドを適用した結果を読める(t *testing.T) {
	f := raftstore.NewFSM()
	b := domain.Base{Name: name("prod"), Status: domain.BaseReady}
	br := domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady, Password: "pw"}
	n := domain.Node{Name: "node1", Address: "192.0.2.10:7070", LastSeen: time.Unix(100, 0).UTC()}
	apply(t, f, raftstore.PutBase(b))
	apply(t, f, raftstore.PutBranch(br))
	apply(t, f, raftstore.PutNode(n))

	st := f.State()
	if !reflect.DeepEqual(st.Bases["prod"], b) || !reflect.DeepEqual(st.Branches["x"], br) || !reflect.DeepEqual(st.Nodes["node1"], n) {
		t.Errorf("state = %+v", st)
	}

	apply(t, f, raftstore.DeleteBranch(name("x")))
	apply(t, f, raftstore.DeleteBase(name("prod")))
	apply(t, f, raftstore.DeleteNode("node1"))
	st = f.State()
	if len(st.Bases)+len(st.Branches)+len(st.Nodes) != 0 {
		t.Errorf("after deletes = %+v", st)
	}
}

func TestFSM_参加用の秘密値のハッシュを保持する(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.SetJoinSecret("hash-1"))
	if f.State().JoinSecretHash != "hash-1" {
		t.Errorf("hash = %q", f.State().JoinSecretHash)
	}
}

func TestFSM_スナップショットから同じ状態を復元する(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("prod"), Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}))
	apply(t, f, raftstore.PutNode(domain.Node{Name: "n1"}))

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := snap.Persist(&sink{Buffer: &buf}); err != nil {
		t.Fatal(err)
	}
	snap.Release()

	g := raftstore.NewFSM()
	if err := g.Restore(io.NopCloser(&buf)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.State(), g.State()) {
		t.Errorf("restored %+v, want %+v", g.State(), f.State())
	}
}

func TestFSM_壊れたコマンドやスナップショットはエラーにする(t *testing.T) {
	f := raftstore.NewFSM()
	if res := f.Apply(&raft.Log{Data: []byte("{")}); res == nil {
		t.Error("broken command: want error")
	}
	if res := f.Apply(&raft.Log{Data: []byte(`{"op":"launch_missiles"}`)}); res == nil {
		t.Error("unknown op: want error")
	}
	if err := f.Restore(io.NopCloser(bytes.NewBufferString("{"))); err == nil {
		t.Error("broken snapshot: want error")
	}
}

type sink struct {
	*bytes.Buffer
	cancelled bool
}

func (s *sink) ID() string    { return "test" }
func (s *sink) Cancel() error { s.cancelled = true; return nil }
func (s *sink) Close() error  { return nil }

type failingSink struct{ sink }

func (f *failingSink) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFSM_スナップショットを書けなければ中止する(t *testing.T) {
	f := raftstore.NewFSM()
	snap, _ := f.Snapshot()
	s := &failingSink{sink: sink{Buffer: &bytes.Buffer{}}}
	if err := snap.Persist(s); err == nil || !s.cancelled {
		t.Errorf("err = %v cancelled = %v", err, s.cancelled)
	}
}

func TestFSM_終わったオペレーションは新しいものだけ残す(t *testing.T) {
	f := raftstore.NewFSM()
	for i := 1; i <= operation.Keep+3; i++ {
		apply(t, f, raftstore.PutOperation(operation.Operation{ID: fmt.Sprint("op", i), State: operation.Succeeded, Seq: uint64(i)}))
	}
	if n := len(f.State().Operations); n != operation.Keep {
		t.Errorf("kept %d", n)
	}
}

func TestFSM_オペレーションのない古いスナップショットも復元できる(t *testing.T) {
	f := raftstore.NewFSM()
	if err := f.Restore(io.NopCloser(bytes.NewBufferString(`{"bases":{},"branches":{},"nodes":{}}`))); err != nil {
		t.Fatal(err)
	}
	apply(t, f, raftstore.PutOperation(operation.Operation{ID: "op1"}))
}

func TestFSM_中身の欠けたコマンドは落ちずにエラーにする(t *testing.T) {
	f := raftstore.NewFSM()
	for _, op := range []string{"insert_base", "put_base", "insert_branch", "put_branch", "put_node", "put_operation", "set_admin", "add_replica", "put_route_user", "put_module", "put_token", "set_base_refresh", "record_base_refresh_run"} {
		res := f.Apply(&raft.Log{Data: []byte(`{"op":"` + op + `"}`)})
		if _, ok := res.(error); !ok {
			t.Errorf("%s without data: got %v", op, res)
		}
	}
}

func TestDecodeCommand_知らないフィールドも元のバイト列のまま書き込む(t *testing.T) {
	in := []byte(`{"op":"put_node","node":{"name":"n1"},"future":{"x":1}}`)
	c, err := raftstore.DecodeCommand(in)
	if err != nil || c.Op != "put_node" || c.Node.Name != "n1" {
		t.Fatalf("decode: %+v %v", c, err)
	}
	out, _ := c.Encode()
	if string(out) != string(in) {
		t.Errorf("encoded %s", out)
	}
	if _, err := raftstore.DecodeCommand([]byte("{")); err == nil {
		t.Error("broken: want error")
	}
}

func TestFSM_レプリカはadd_replicaでだけ変わりput_baseでは消えない(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1", Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}))
	apply(t, f, raftstore.AddReplica(name("p"), "s1", "n2"))
	apply(t, f, raftstore.AddReplica(name("p"), "s1", "n2"))
	stale := domain.Base{Name: name("p"), Node: "n1", Status: domain.BaseReady, Replicas: map[string][]string{"s1": {"bogus"}}}
	apply(t, f, raftstore.PutBase(stale))
	b := f.State().Bases["p"]
	if b.Status != domain.BaseReady || len(b.Replicas["s1"]) != 1 || b.Replicas["s1"][0] != "n2" {
		t.Errorf("base = %+v", b)
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.AddReplica(name("gone"), "s1", "n2"))}); res == nil {
		t.Error("replica of a missing base: want error")
	}
	// State() hands out copies.
	f.State().Bases["p"].Replicas["s1"][0] = "mutated"
	if f.State().Bases["p"].Replicas["s1"][0] != "n2" {
		t.Error("State shares replica slices")
	}
}

func mustEncode(c raftstore.Command) []byte { b, _ := c.Encode(); return b }

func TestProtocol_すべてのコマンドに必要な版があり最新の版を超えない(t *testing.T) {
	for _, op := range []string{"put_base", "delete_base", "put_branch", "delete_branch", "put_node", "delete_node",
		"set_join_secret", "insert_base", "insert_branch", "put_operation", "set_admin", "init_admin", "add_replica",
		"add_runtime", "remove_runtime", "put_route_user", "delete_route_user", "put_module", "put_settings", "put_token", "delete_token",
		"put_module_data", "delete_module_data", "start_trial", "set_base_refresh", "record_base_refresh_run", "clear_base_refresh", "set_base_replica_nodes", "remove_replica", "set_base_anonymous"} {
		n := raftstore.Command{Op: op}.Needs()
		if n < 1 || n > raftstore.Protocol {
			t.Errorf("%s needs %d", op, n)
		}
	}
}

func TestFSM_更新スケジュールは別のコマンドで保存され最後の実行を保つ(t *testing.T) {
	f := raftstore.NewFSM()
	r := domain.BaseRefresh{Base: name("p"), Every: 2 * time.Hour, Source: domain.RefreshSource{Host: "h", Databases: []string{"a"}}}
	apply(t, f, raftstore.SetBaseRefresh(r))
	run := domain.RefreshRun{OK: true, FinishedAt: time.Unix(10, 0).UTC()}
	apply(t, f, raftstore.RecordBaseRefreshRun(name("p"), run))
	r.Every = 3 * time.Hour
	apply(t, f, raftstore.SetBaseRefresh(r))
	got := f.State().BaseRefreshes["p"]
	if got.Every != 3*time.Hour || got.LastRun == nil || !got.LastRun.OK {
		t.Errorf("refresh = %+v", got)
	}
	// State() hands out copies.
	f.State().BaseRefreshes["p"].Source.Databases[0] = "mutated"
	f.State().BaseRefreshes["p"].LastRun.OK = false
	if again := f.State().BaseRefreshes["p"]; again.Source.Databases[0] != "a" || !again.LastRun.OK {
		t.Error("State shares refresh data")
	}
	// A schedule survives a snapshot, and a missing one cannot get a run.
	snap, _ := f.Snapshot()
	var buf bytes.Buffer
	_ = snap.Persist(&sink{Buffer: &buf})
	g := raftstore.NewFSM()
	if err := g.Restore(io.NopCloser(&buf)); err != nil || g.State().BaseRefreshes["p"].Every != 3*time.Hour {
		t.Errorf("restore: %v %+v", err, g.State().BaseRefreshes)
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.RecordBaseRefreshRun(name("gone"), run))}); res == nil {
		t.Error("run of a missing schedule: want error")
	}
	apply(t, f, raftstore.ClearBaseRefresh(name("p")))
	if len(f.State().BaseRefreshes) != 0 {
		t.Error("not cleared")
	}
}

func TestFSM_ベースを消すと更新スケジュールも消える(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p")}))
	apply(t, f, raftstore.SetBaseRefresh(domain.BaseRefresh{Base: name("p")}))
	apply(t, f, raftstore.DeleteBase(name("p")))
	if len(f.State().BaseRefreshes) != 0 {
		t.Error("schedule outlived its base")
	}
}

func TestFSM_ロールアウトは一つずつ走り_進み具合を記録して_終える(t *testing.T) {
	f := raftstore.NewFSM()
	at := time.Unix(10, 0).UTC()
	r := domain.NewRollout("r1", "1.1.0", []string{"b", "a"}, map[string]string{"a": "1.0.0", "b": "1.0.0"}, at)
	apply(t, f, raftstore.StartRollout(r))
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.StartRollout(r))}); res == nil {
		t.Error("a second rollout must not start while one runs")
	}
	apply(t, f, raftstore.RecordRollout(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeUpdating, At: at}))
	if got := f.State().Rollout; got == nil || got.Nodes["b"].State != domain.NodeUpdating {
		t.Fatalf("rollout = %+v", got)
	}
	// The copy handed out does not alias the state.
	f.State().Rollout.Nodes["b"] = domain.NodeUpdate{State: domain.NodeDone}
	if f.State().Rollout.Nodes["b"].State != domain.NodeUpdating {
		t.Error("State shares the rollout's map")
	}
	// The stored rollout does not alias the command's.
	r.Nodes["a"] = domain.NodeUpdate{State: domain.NodeDone}
	if f.State().Rollout.Nodes["a"].State != domain.NodePending {
		t.Error("state shares the command's map")
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.RecordRollout(domain.RolloutEvent{Rollout: "old", Node: "b", State: domain.NodeDone}))}); res == nil {
		t.Error("an event for another rollout must fail")
	}
	apply(t, f, raftstore.RecordRollout(domain.RolloutEvent{Rollout: "r1", Status: domain.RolloutCanceled, At: at, Error: "stopped"}))
	if f.State().Rollout.Status != domain.RolloutCanceled {
		t.Errorf("rollout = %+v", f.State().Rollout)
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.RecordRollout(domain.RolloutEvent{Rollout: "r1", Status: domain.RolloutDone}))}); res == nil {
		t.Error("ending twice must fail")
	}
	apply(t, f, raftstore.StartRollout(domain.NewRollout("r2", "1.2.0", []string{"a"}, map[string]string{"a": "1.0.0"}, at)))
	var buf bytes.Buffer
	snap, _ := f.Snapshot()
	_ = snap.Persist(&sink{Buffer: &buf})
	g := raftstore.NewFSM()
	if err := g.Restore(io.NopCloser(&buf)); err != nil || g.State().Rollout == nil || g.State().Rollout.ID != "r2" {
		t.Errorf("restore: %v %+v", err, g.State().Rollout)
	}
}

func TestFSM_更新の確認結果を記録する(t *testing.T) {
	f := raftstore.NewFSM()
	c := domain.UpdateCheck{At: time.Unix(5, 0).UTC(), By: "n1", Packages: map[string][]domain.PackageVersion{"db-branch": {{Version: "1.1.0"}}}}
	apply(t, f, raftstore.PutUpdateCheck(c))
	if got := f.State().Update; got == nil || got.By != "n1" || got.Packages["db-branch"][0].Version != "1.1.0" {
		t.Errorf("check = %+v", got)
	}
	f.State().Update.Packages["db-branch"][0].Version = "x"
	if f.State().Update.Packages["db-branch"][0].Version != "1.1.0" {
		t.Error("State shares the check")
	}
	for _, c := range []raftstore.Command{{Op: "put_update_check"}, {Op: "start_rollout"}, {Op: "record_rollout"}} {
		if res := f.Apply(&raft.Log{Data: mustEncode(c)}); res == nil {
			t.Errorf("%s without its data must fail", c.Op)
		}
	}
}

func TestFSM_レプリカ先の設定はput_baseで消えず空リストと未設定を区別しコピーを返す(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1", Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}))
	if b := f.State().Bases["p"]; b.ReplicaNodes != nil {
		t.Fatalf("default must be every node: %v", b.ReplicaNodes)
	}
	apply(t, f, raftstore.SetBaseReplicaNodes(name("p"), &[]string{}))
	data, _ := raftstore.SetBaseReplicaNodes(name("p"), &[]string{}).Encode()
	if !strings.Contains(string(data), `"nodes":[]`) {
		t.Errorf("empty list must be written as []: %s", data)
	}
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1", Status: domain.BaseReady}))
	if b := f.State().Bases["p"]; b.ReplicaNodes == nil || len(*b.ReplicaNodes) != 0 || b.Status != domain.BaseReady {
		t.Errorf("put_base lost the setting: %+v", b)
	}
	apply(t, f, raftstore.SetBaseReplicaNodes(name("p"), &[]string{"n2"}))
	(*f.State().Bases["p"].ReplicaNodes)[0] = "mutated"
	if (*f.State().Bases["p"].ReplicaNodes)[0] != "n2" {
		t.Error("State shares the list")
	}
	apply(t, f, raftstore.SetBaseReplicaNodes(name("p"), nil))
	if f.State().Bases["p"].ReplicaNodes != nil {
		t.Error("null must mean every node again")
	}
	apply(t, f, raftstore.SetBaseReplicaNodes(name("p"), &[]string{"n2"}))
	apply(t, f, raftstore.InsertBase(domain.Base{Name: name("q"), ReplicaNodes: &[]string{"x"}}))
	if f.State().Bases["q"].ReplicaNodes != nil {
		t.Error("insert_base must not carry the setting")
	}
	apply(t, f, raftstore.DeleteBase(name("p")))
	if _, ok := f.State().Bases["p"]; ok {
		t.Error("delete_base drops the base with its setting")
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.SetBaseReplicaNodes(name("gone"), nil))}); res == nil {
		t.Error("setting of a missing base: want error")
	}
}

func TestFSM_remove_replicaはそのノードの記録だけを消す(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1"}))
	apply(t, f, raftstore.AddReplica(name("p"), "s1", "n2"))
	apply(t, f, raftstore.AddReplica(name("p"), "s1", "n3"))
	apply(t, f, raftstore.RemoveReplica(name("p"), "n2"))
	if r := f.State().Bases["p"].Replicas["s1"]; len(r) != 1 || r[0] != "n3" {
		t.Errorf("replicas = %v", r)
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.RemoveReplica(name("gone"), "n2"))}); res == nil {
		t.Error("missing base: want error")
	}
}

func TestFSM_匿名の上限はコマンドで保存されput_baseで消えず継承に戻せコピーを返す(t *testing.T) {
	f := raftstore.NewFSM()
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1"}))
	if b := f.State().Bases["p"]; !b.Anonymous.Inherits() {
		t.Fatalf("default must inherit: %+v", b.Anonymous)
	}
	three := 3
	cmd := raftstore.SetBaseAnonymous(name("p"), domain.AnonymousLimits{MaxBranches: &three, MaxTTL: 4 * time.Hour})
	// The command survives a trip through the log (encode, decode, apply).
	data, err := cmd.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := raftstore.DecodeCommand(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Op != "set_base_anonymous" || back.Needs() != 14 {
		t.Errorf("op %q needs protocol %d", back.Op, back.Needs())
	}
	apply(t, f, back)
	b := f.State().Bases["p"]
	if b.Anonymous.MaxBranches == nil || *b.Anonymous.MaxBranches != 3 || b.Anonymous.MaxTTL != 4*time.Hour {
		t.Fatalf("not stored: %+v", b.Anonymous)
	}
	// A base update written from an older read must not drop the limits.
	apply(t, f, raftstore.PutBase(domain.Base{Name: name("p"), Node: "n1", Status: domain.BaseReady}))
	if b = f.State().Bases["p"]; b.Anonymous.MaxBranches == nil || b.Status != domain.BaseReady {
		t.Errorf("put_base lost the limits: %+v", b)
	}
	// State() hands out copies.
	*f.State().Bases["p"].Anonymous.MaxBranches = 99
	if *f.State().Bases["p"].Anonymous.MaxBranches != 3 {
		t.Error("State shares the limit")
	}
	// 0 (no anonymous branches) is different from inherit.
	zero := 0
	apply(t, f, raftstore.SetBaseAnonymous(name("p"), domain.AnonymousLimits{MaxBranches: &zero}))
	if b = f.State().Bases["p"]; b.Anonymous.MaxBranches == nil || !b.Anonymous.Disabled() {
		t.Errorf("0 must be kept: %+v", b.Anonymous)
	}
	// It is part of the snapshot.
	snap, _ := f.Snapshot()
	var buf bytes.Buffer
	if err := snap.Persist(&sink{Buffer: &buf}); err != nil {
		t.Fatal(err)
	}
	g := raftstore.NewFSM()
	if err := g.Restore(io.NopCloser(&buf)); err != nil || !g.State().Bases["p"].Anonymous.Disabled() {
		t.Errorf("restore: %v %+v", err, g.State().Bases["p"].Anonymous)
	}
	apply(t, f, raftstore.SetBaseAnonymous(name("p"), domain.AnonymousLimits{}))
	if !f.State().Bases["p"].Anonymous.Inherits() {
		t.Error("the zero value must inherit again")
	}
	apply(t, f, raftstore.SetBaseAnonymous(name("p"), domain.AnonymousLimits{MaxBranches: &three}))
	apply(t, f, raftstore.InsertBase(domain.Base{Name: name("q"), Anonymous: domain.AnonymousLimits{MaxBranches: &three}}))
	if !f.State().Bases["q"].Anonymous.Inherits() {
		t.Error("insert_base must not carry the limits")
	}
	if res := f.Apply(&raft.Log{Data: mustEncode(raftstore.SetBaseAnonymous(name("gone"), domain.AnonymousLimits{}))}); res == nil {
		t.Error("limits of a missing base: want error")
	}
}

func TestFSM_ノードのシステム情報とスナップショットの作成元ディストリビューションを保持する(t *testing.T) {
	// Both travel inside put_node and put_base: no new command, no Protocol bump.
	f := raftstore.NewFSM()
	n := domain.Node{Name: "node4", System: domain.NodeSystem{Distro: "ubuntu-26.04", OS: "Ubuntu 26.04.1 LTS", Kernel: "7.0.0-14-generic", ZFS: "2.4.0"}}
	b := domain.Base{Name: name("prod"), Snapshots: []domain.BaseSnapshot{{Name: "s1", Distro: "ubuntu-24.04"}}}
	apply(t, f, raftstore.PutNode(n))
	apply(t, f, raftstore.PutBase(b))
	st := f.State()
	if st.Nodes["node4"].System != n.System || st.Bases["prod"].Snapshots[0].Distro != "ubuntu-24.04" {
		t.Errorf("state = %+v", st)
	}
}

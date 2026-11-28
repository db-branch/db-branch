package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestCompareVersions_Debianの順序で比べる(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.1.0", -1},
		{"1.10.0", "1.9.0", 1},
		{"1.0.0~rc1", "1.0.0", -1},
		{"1.0.0~rc1", "1.0.0~rc2", -1},
		{"1.0.0+b1", "1.0.0", 1},
		{"1.0.0-1", "1.0.0-2", -1},
		{"1.0.0", "1.0.0-1", -1},
		{"1:0.1", "5.0", 1},
		{"0.0.0~dev20261111070644.ee06010", "0.0.0~dev20261112000000.aaaaaaa", -1},
		{"0.0.0~dev20261111070644.ee06010", "0.1.0", -1},
		{"1.0a", "1.0", 1},
		{"1.0a", "1.0b", -1},
		{"1.0.0", "1.0.0.0", -1},
	} {
		if got := domain.CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := domain.CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestUpdateKind_同じメジャーまたは0から1の新しい版を判定する(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"1.2.0", "1.3.0", domain.UpdateMinor},
		{"1.2.0", "1.2.1", domain.UpdateMinor},
		{"1.2.0", "2.0.0", domain.UpdateMajor},
		{"1.2.0", "1.2.0", ""},
		{"1.2.0", "1.1.0", ""},
		{"2.0.0", "1.9.0", ""},
		{"0.0.0~dev20261111070644.ee06010", "0.0.0~dev20261112000000.aaaaaaa", domain.UpdateMinor},
		{"0.4.1", "0.5.0", domain.UpdateMinor},
		{"0.4.1", "1.0.0", domain.UpdateMajor0},
		{"0.4.1", "1.2.0", domain.UpdateMajor0},
		{"0.4.1", "1.0.0~rc1", domain.UpdateMajor0},
		{"0.0.0~dev20261128000000.aaaaaaa", "1.0.0~dev20261129000000.bbbbbbb", domain.UpdateMajor0},
		{"0.4.1", "2.0.0", domain.UpdateMajor},
		{"1.0.0~rc1", "1.0.0", domain.UpdateMinor},
		{"1.0.0", "0.4.1", ""},
		{"0.4.1", "1:1.0.0", domain.UpdateMajor},
		{"1:1.2.0", "1:1.3.0", domain.UpdateMinor},
		{"1:0.4.1", "1:1.0.0", domain.UpdateMajor0},
		{"x", "1.0", ""},
	} {
		if got := domain.UpdateKind(c.from, c.to); got != c.want {
			t.Errorf("UpdateKind(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
	for _, c := range []struct {
		kind string
		want bool
	}{
		{domain.UpdateMinor, true},
		{domain.UpdateMajor0, true},
		{domain.UpdateMajor, false},
		{"", false},
	} {
		if got := domain.UpdateApplicable(c.kind); got != c.want {
			t.Errorf("UpdateApplicable(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
	for _, c := range []struct {
		v    string
		want string
		ok   bool
	}{
		{"0.4.1", "0", true},
		{"00.4.1", "0", true},
		{"1.0.0", "1", true},
		{"1:0.4.1", "0", true},
		{"x", "", false},
	} {
		if got, ok := domain.Major(c.v); got != c.want || ok != c.ok {
			t.Errorf("Major(%q) = (%q, %v), want (%q, %v)", c.v, got, ok, c.want, c.ok)
		}
	}
}

func node(name, version string, seen time.Time) domain.Node {
	return domain.Node{Name: name, Version: version, LastSeen: seen}
}

func newRollout() domain.Rollout {
	return domain.NewRollout("r1", "1.1.0", []string{"b", "c", "a"}, map[string]string{"a": "1.0.0", "b": "1.0.0", "c": "1.1.0"}, time.Unix(100, 0))
}

func TestNewRollout_すでに目標の版のノードは済みにする(t *testing.T) {
	r := newRollout()
	if r.Status != domain.RolloutRunning || r.Nodes["c"].State != domain.NodeDone || r.Nodes["a"].State != domain.NodePending || r.Nodes["a"].From != "1.0.0" {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTurn_順番が来たノードだけが進める(t *testing.T) {
	now := time.Unix(1000, 0)
	ok := func(name, v string) domain.Node { return node(name, v, now) }
	nodes := []domain.Node{ok("a", "1.0.0"), ok("b", "1.0.0"), ok("c", "1.1.0")}
	r := newRollout()
	if got := r.Turn(nodes, now, 30*time.Second); got != "b" {
		t.Errorf("first turn = %q, want b", got)
	}
	_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeUpdating, At: now})
	if got := r.Turn(nodes, now, 30*time.Second); got != "" {
		t.Errorf("while b updates, turn = %q", got)
	}
	_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeDone, At: now})
	// b is done but does not report the target yet.
	if got := r.Turn(nodes, now, 30*time.Second); got != "" {
		t.Errorf("b still reports the old version, turn = %q", got)
	}
	nodes[1].Version = "1.1.0"
	if got := r.Turn(nodes, now, 30*time.Second); got != "a" {
		t.Errorf("turn = %q, want a (the leader is last)", got)
	}
	// An unhealthy predecessor holds everyone back.
	nodes[1].LastSeen = now.Add(-time.Minute)
	if got := r.Turn(nodes, now, 30*time.Second); got != "" {
		t.Errorf("b is down, turn = %q", got)
	}
	nodes = nodes[:1]
	if got := r.Turn(nodes, now, 30*time.Second); got != "" {
		t.Errorf("a predecessor left the cluster, turn = %q", got)
	}
}

func TestTurn_終わった巻き戻しや失敗では誰の番でもない(t *testing.T) {
	now := time.Unix(1000, 0)
	nodes := []domain.Node{node("a", "1.0.0", now), node("b", "1.0.0", now), node("c", "1.1.0", now)}
	r := newRollout()
	_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeUpdating, At: now})
	_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeFailed, At: now, Error: "boom"})
	if r.Status != domain.RolloutFailed || r.Error == "" {
		t.Errorf("a failed node fails the rollout: %+v", r)
	}
	if got := r.Turn(nodes, now, time.Minute); got != "" {
		t.Errorf("turn = %q", got)
	}
}

func TestApply_不正な遷移は断る(t *testing.T) {
	at := time.Unix(5, 0)
	r := newRollout()
	for _, ev := range []domain.RolloutEvent{
		{Rollout: "other", Node: "a", State: domain.NodeUpdating, At: at},
		{Rollout: "r1", Node: "zz", State: domain.NodeUpdating, At: at},
		{Rollout: "r1", Node: "a", State: domain.NodeDone, At: at},
		{Rollout: "r1", Node: "a", State: domain.NodePending, At: at},
	} {
		if err := r.Apply(ev); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("Apply(%+v) = %v, want ErrConflict", ev, err)
		}
	}
	if err := r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "a", State: domain.NodeUpdating, At: at}); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "a", State: domain.NodeUpdating, At: at}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("updating twice: %v", err)
	}
	if n := r.Nodes["a"]; n.StartedAt != at {
		t.Errorf("node = %+v", n)
	}
}

func TestEnd_走っているロールアウトだけを終えられる(t *testing.T) {
	at := time.Unix(9, 0)
	r := newRollout()
	if err := r.End(domain.RolloutCanceled, "stopped", at); err != nil || r.Status != domain.RolloutCanceled || r.FinishedAt != at || r.Error != "stopped" {
		t.Errorf("End = %v %+v", err, r)
	}
	if err := r.End(domain.RolloutDone, "", at); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("ending twice: %v", err)
	}
	if err := (&domain.Rollout{Status: domain.RolloutRunning}).End(domain.RolloutRunning, "", at); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("end with a non-final status: %v", err)
	}
}

func TestApply_キャンセル後も動いていたノードの結果は残る(t *testing.T) {
	at := time.Unix(9, 0)
	r := newRollout()
	_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeUpdating, At: at})
	_ = r.End(domain.RolloutCanceled, "stopped", at)
	if err := r.Apply(domain.RolloutEvent{Rollout: "r1", Node: "b", State: domain.NodeDone, At: at}); err != nil || r.Status != domain.RolloutCanceled || r.Nodes["b"].State != domain.NodeDone {
		t.Errorf("after cancel: %v %+v", err, r)
	}
}

func TestComplete_全ノードが済んだとき(t *testing.T) {
	at := time.Unix(9, 0)
	r := newRollout()
	if r.Complete() {
		t.Error("not complete yet")
	}
	for _, n := range []string{"b", "a"} {
		_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: n, State: domain.NodeUpdating, At: at})
		_ = r.Apply(domain.RolloutEvent{Rollout: "r1", Node: n, State: domain.NodeDone, At: at})
	}
	if !r.Complete() {
		t.Errorf("complete expected: %+v", r)
	}
}

func TestClone_コピーは元と共有しない(t *testing.T) {
	r := newRollout()
	r.Modules = map[string]string{"db-branch-auth": "1.1.0"}
	c := r.Clone()
	c.Nodes["a"] = domain.NodeUpdate{State: domain.NodeDone}
	c.Modules["x"] = "1"
	c.Order[0] = "zz"
	if r.Nodes["a"].State != domain.NodePending || len(r.Modules) != 1 || r.Order[0] != "b" {
		t.Errorf("clone shares memory: %+v", r)
	}
}

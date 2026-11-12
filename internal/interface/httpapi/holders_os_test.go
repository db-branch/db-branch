package httpapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
)

var pg18 = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}

// mixedFixture: n1 and n2 run Ubuntu 24.04, n3 runs 26.04; the base lives on n2.
func mixedFixture(t *testing.T, rt domain.RuntimeRef, snapDistro string, replicaNodes *[]string) *fixture {
	t.Helper()
	f, c := remoteFixture(t)
	sys := func(d string) domain.NodeSystem { return domain.NodeSystem{Distro: d} }
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n1", Nodes: []httpapi.NodeStatus{
		{Name: "n1", System: sys("ubuntu-24.04")}, {Name: "n2", System: sys("ubuntu-24.04")}, {Name: "n3", System: sys("ubuntu-26.04")},
	}}
	f.branches.bases["far"] = domain.Base{Name: name("far"), Node: "n2", Runtime: rt, Status: domain.BaseReady,
		Snapshots: []domain.BaseSnapshot{{Name: "s1", Distro: snapDistro}}, ReplicaNodes: replicaNodes}
	return f
}

func holderStates(t *testing.T, f *fixture) string {
	t.Helper()
	_, body := f.do(t, "GET", "/v1/bases/far", nil)
	var parts []string
	for _, h := range body["holders"].([]any) {
		m := h.(map[string]any)
		parts = append(parts, fmt.Sprintf("%s=%s", m["node"], m["state"]))
		if m["state"] == "skipped" {
			parts = append(parts, fmt.Sprintf("(%v|%v)", m["reason"], m["distro"]))
		}
	}
	return strings.Join(parts, " ")
}

func TestHolders_記録のない旧スナップショットは担当ノードのOSと違うノードを対象外にする(t *testing.T) {
	f := mixedFixture(t, pg18, "", nil)
	want := "n2=held n1=pulling n3=skipped (other OS: ubuntu-26.04|ubuntu-26.04)"
	if got := holderStates(t, f); got != want {
		t.Errorf("holders = %s, want %s", got, want)
	}
}

func TestHolders_記録されたOSがあれば担当ノードの現在のOSより優先する(t *testing.T) {
	f := mixedFixture(t, pg18, "ubuntu-26.04", nil)
	want := "n2=held n1=skipped (other OS: ubuntu-24.04|ubuntu-24.04) n3=pulling"
	if got := holderStates(t, f); got != want {
		t.Errorf("holders = %s, want %s", got, want)
	}
}

func TestHolders_MySQLはOSが違っても対象外にしない(t *testing.T) {
	f := mixedFixture(t, mysql84, "", nil)
	if got, want := holderStates(t, f), "n2=held n1=pulling n3=pulling"; got != want {
		t.Errorf("holders = %s, want %s", got, want)
	}
}

func TestHolders_レプリカ先の明示一覧でも別OSのノードは対象外(t *testing.T) {
	list := []string{"n1", "n3"}
	f := mixedFixture(t, pg18, "", &list)
	want := "n2=held n1=pulling n3=skipped (other OS: ubuntu-26.04|ubuntu-26.04)"
	if got := holderStates(t, f); got != want {
		t.Errorf("holders = %s, want %s", got, want)
	}
}

func TestHolders_OSを報告しないノードと既にコピーを持つノードは対象外にしない(t *testing.T) {
	f := mixedFixture(t, pg18, "", nil)
	b := f.branches.bases["far"]
	b.Replicas = map[string][]string{"s1": {"n3"}}
	f.branches.bases["far"] = b
	if got, want := holderStates(t, f), "n2=held n1=pulling n3=held"; got != want {
		t.Errorf("holders = %s, want %s", got, want)
	}
	b.Replicas = nil
	f.branches.bases["far"] = b
	f.srv.Cluster.(*fakeCluster).status.Nodes[2].System = domain.NodeSystem{}
	if got, want := holderStates(t, f), "n2=held n1=pulling n3=pulling"; got != want {
		t.Errorf("unknown OS: holders = %s, want %s", got, want)
	}
}

func TestHolders_単独ノードでは自分のOSを元に判断する(t *testing.T) {
	f := newFixture(t)
	f.srv.System = func(context.Context) domain.NodeSystem { return domain.NodeSystem{Distro: "ubuntu-24.04"} }
	only := []string{"x"}
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: pg18, Status: domain.BaseReady, Node: "node1",
		Snapshots: []domain.BaseSnapshot{{Name: "s1"}}, ReplicaNodes: &only}
	_, body := f.do(t, "GET", "/v1/bases/prod", nil)
	if got := fmt.Sprint(body["holders"]); !strings.Contains(got, "node:x origin:false state:pulling") {
		t.Errorf("holders = %s", got)
	}
}

func TestMetrics_複製の遅れに別OSのノードと対象外のノードを数えない(t *testing.T) {
	f := mixedFixture(t, pg18, "", nil)
	if _, out := scrape(t, f); !strings.Contains(out, `dbbranch_base_replicas_missing{base="far"} 1`) {
		t.Errorf("only n1 is missing:\n%s", out)
	}
	list := []string{}
	b := f.branches.bases["far"]
	b.ReplicaNodes = &list
	f.branches.bases["far"] = b
	if _, out := scrape(t, f); !strings.Contains(out, `dbbranch_base_replicas_missing{base="far"} 0`) {
		t.Errorf("origin only wants no copies:\n%s", out)
	}
	list = []string{"n1"}
	b.ReplicaNodes = &list
	f.branches.bases["far"] = b
	if _, out := scrape(t, f); !strings.Contains(out, `dbbranch_base_replicas_missing{base="far"} 1`) {
		t.Errorf("n1 is wanted and missing:\n%s", out)
	}
	my := mixedFixture(t, mysql84, "", nil)
	if _, out := scrape(t, my); !strings.Contains(out, `dbbranch_base_replicas_missing{base="far"} 2`) {
		t.Errorf("MySQL counts every node:\n%s", out)
	}
}

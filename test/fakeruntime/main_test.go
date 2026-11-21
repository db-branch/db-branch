package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

func TestDescribe_upgradeを宣言する(t *testing.T) {
	spec, err := (module{}).Describe(context.Background(), &pb.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.GetCapabilities().GetQuery() || !spec.GetCapabilities().GetUpgrade() {
		t.Fatalf("capabilities = %+v, want query=true and upgrade=true", spec.GetCapabilities())
	}
}

func TestCheckUpgradePath_新しい版のみ許可する(t *testing.T) {
	m := module{}
	ctx := context.Background()
	for _, tc := range []struct {
		from, to string
		wantOK   bool
	}{
		{"1.0", "1.1", true},
		{"1.0", "2.0", true},
		{"1.9", "1.10", true},
		{"1.0", "1.0", false},
		{"2.0", "1.0", false},
		{"1.10", "1.2", false},
		{"bad", "2.0", false},
		{"1.0", "bad", false},
	} {
		_, err := m.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: tc.from, ToVersion: tc.to})
		if tc.wantOK && err != nil {
			t.Errorf("%s -> %s: unexpected error: %v", tc.from, tc.to, err)
		}
		if !tc.wantOK {
			if err == nil {
				t.Errorf("%s -> %s: expected error, got nil", tc.from, tc.to)
			} else if status.Code(err) == codes.Unimplemented {
				t.Errorf("%s -> %s: got Unimplemented", tc.from, tc.to)
			}
		}
	}
}

func TestCheckUpgrade_常に空のUpgradePlanを返す(t *testing.T) {
	plan, err := (module{}).CheckUpgrade(context.Background(), &pb.CheckUpgradeRequest{
		Instance:  &pb.InstanceSpec{Id: "x", Version: "1.0"},
		ToVersion: "2.0",
		Login:     &pb.Login{User: "app", Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.GetFindings()) != 0 || plan.GetLegacyAuth() {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}

func TestStartUpgraded_PrepareStartと同じLaunchPlanと空のログを返す(t *testing.T) {
	m := module{}
	ctx := context.Background()
	dir := t.TempDir()
	inst := &pb.InstanceSpec{
		Id:         "rtit-1",
		Engine:     "fakedb",
		Version:    "2.0",
		RuntimeDir: "/var/lib/db-branch/runtimes/fakedb/2.0",
		DataDir:    filepath.Join(dir, "data"),
		StateDir:   filepath.Join(dir, "state"),
		Bind:       "127.0.0.1",
		Ports:      map[string]int32{"sql": 38999},
	}
	if err := os.MkdirAll(inst.StateDir, 0o750); err != nil {
		t.Fatal(err)
	}

	wantPlan, err := m.PrepareStart(ctx, &pb.InstanceRequest{Instance: inst})
	if err != nil {
		t.Fatalf("PrepareStart: %v", err)
	}
	res, err := m.StartUpgraded(ctx, &pb.StartUpgradedRequest{Instance: inst, Login: &pb.Login{User: "app", Password: "pw"}})
	if err != nil {
		t.Fatalf("StartUpgraded: %v", err)
	}
	if !reflect.DeepEqual(res.GetPlan(), wantPlan) {
		t.Fatalf("plan = %+v, want %+v", res.GetPlan(), wantPlan)
	}
	if len(res.GetFindings()) != 0 || res.GetExcerpt() != "" {
		t.Fatalf("log = findings=%v excerpt=%q, want empty", res.GetFindings(), res.GetExcerpt())
	}
	conf, err := os.ReadFile(filepath.Join(inst.StateDir, "fakedb.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "runtime_dir=/var/lib/db-branch/runtimes/fakedb/2.0") {
		t.Fatalf("fakedb.conf = %q, want runtime_dir", string(conf))
	}
}

package runtimegrpc_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
	"github.com/db-branch/db-branch/pkg/ext/runtimeplugin"
)

// The test binary doubles as a runtime module: with DBB_TEST_MODULE set it
// serves a fake engine instead of running the tests.
func TestMain(m *testing.M) {
	if os.Getenv("DBB_TEST_MODULE") == "broken" {
		runtimeplugin.Serve(fake{describe: func() (*pb.EngineSpec, error) { return nil, status.Error(codes.Internal, "cannot describe") }})
		return
	}
	if os.Getenv("DBB_TEST_MODULE") == "1" {
		runtimeplugin.Serve(fake{describe: func() (*pb.EngineSpec, error) {
			return &pb.EngineSpec{Engine: "fakeengine", DisplayName: "Fake", Protocols: []*pb.ProtocolPort{{Name: "sql", Primary: true}}}, nil
		}})
		return
	}
	os.Exit(m.Run())
}

func moduleCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "DBB_TEST_MODULE=1")
	return cmd
}

func TestLaunch_別プロセスのモジュールをgRPCで呼べる(t *testing.T) {
	p, stop, err := runtimegrpc.Launch(moduleCmd())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	spec, err := p.Describe(context.Background())
	if err != nil || spec.Engine != "fakeengine" || len(spec.Protocols) != 1 {
		t.Fatalf("%+v %v", spec, err)
	}
	// An operation the module does not implement is "not supported", not a crash.
	if err := p.UpdateStats(context.Background(), domain.InstanceSpec{ID: "x"}); err == nil {
		t.Fatal("expected not supported")
	}
}

func TestLaunch_停止するとモジュールのプロセスが終わる(t *testing.T) {
	p, stop, err := runtimegrpc.Launch(moduleCmd())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.Describe(ctx); err == nil {
		t.Fatal("a stopped module must not answer")
	}
}

func TestLaunch_モジュールでないバイナリは起動を拒否する(t *testing.T) {
	// No magic cookie handling: the process exits at once.
	if _, _, err := runtimegrpc.Launch(exec.Command("/bin/true")); err == nil {
		t.Fatal("a binary that is not a runtime module must be refused")
	}
}

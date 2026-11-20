package runtimegrpc_test

import (
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// connect serves srv in memory and returns a Provider talking to it.
func connect(t *testing.T, srv pb.RuntimeServer) *runtimegrpc.Provider {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	pb.RegisterRuntimeServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return runtimegrpc.New(conn)
}

type fake struct {
	pb.UnimplementedRuntimeServer
	describe func() (*pb.EngineSpec, error)
}

func (f fake) Describe(context.Context, *pb.DescribeRequest) (*pb.EngineSpec, error) {
	return f.describe()
}

func TestDescribe_モジュールの宣言をEngineSpecに写せる(t *testing.T) {
	p := connect(t, fake{describe: func() (*pb.EngineSpec, error) {
		return &pb.EngineSpec{
			Engine: "clickhouse", DisplayName: "ClickHouse", CollationBound: true, RunAsUser: "db-branch-rt-clickhouse",
			VersionPattern: `^[0-9]+\.[0-9]+$`,
			ZfsProps:       map[string]string{"recordsize": "128K"},
			Protocols:      []*pb.ProtocolPort{{Name: "http", Primary: true}, {Name: "native"}},
			Capabilities:   &pb.Capabilities{Query: true, Checkpoints: true},
		}, nil
	}})
	got, err := p.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine != "clickhouse" || got.DisplayName != "ClickHouse" || !got.CollationBound || got.RunAsUser != "db-branch-rt-clickhouse" ||
		got.VersionPattern != `^[0-9]+\.[0-9]+$` || got.ZFSProps["recordsize"] != "128K" {
		t.Fatalf("spec = %+v", got)
	}
	if !reflect.DeepEqual(got.Protocols, []domain.ProtocolPort{{Name: "http", Primary: true}, {Name: "native"}}) {
		t.Fatalf("protocols = %+v", got.Protocols)
	}
	if !got.Capabilities.Query || !got.Capabilities.Checkpoints || got.Capabilities.Upgrade {
		t.Fatalf("capabilities = %+v", got.Capabilities)
	}
}

func Testエラー_UNIMPLEMENTEDは機能非対応として扱える(t *testing.T) {
	p := connect(t, fake{}) // every other method is unimplemented
	err := p.UpdateStats(context.Background(), domain.InstanceSpec{ID: "x"})
	if !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Fatalf("err = %v", err)
	}
}

func Testエラー_その他の失敗は元のメッセージを保つ(t *testing.T) {
	p := connect(t, fake{describe: func() (*pb.EngineSpec, error) { return nil, status.Error(codes.Internal, "disk on fire") }})
	_, err := p.Describe(context.Background())
	if err == nil || errors.Is(err, domain.ErrFeatureNotSupported) || err.Error() == "" {
		t.Fatalf("err = %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "disk on fire") {
		t.Fatalf("message lost: %q", got)
	}
}

// AdminAccess lets the integration test see who the module process runs as and
// what it inherited.
func (f fake) AdminAccess(context.Context, *pb.InstanceRequest) (*pb.AdminAccessInfo, error) {
	return &pb.AdminAccessInfo{
		User: strconv.Itoa(os.Getuid()), Host: strconv.Itoa(os.Getgid()),
		Env: map[string]string{"DB_BRANCH_SECRET": os.Getenv("DB_BRANCH_SECRET"), "HOME": os.Getenv("HOME")},
	}, nil
}

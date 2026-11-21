package app

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

type fakeUpgradeModule struct {
	pb.UnimplementedRuntimeServer
	spec      *pb.EngineSpec
	pathCalls [][2]string
	pathErr   error
}

func (f *fakeUpgradeModule) Describe(context.Context, *pb.DescribeRequest) (*pb.EngineSpec, error) {
	return f.spec, nil
}

func (f *fakeUpgradeModule) CheckUpgradePath(_ context.Context, r *pb.CheckUpgradePathRequest) (*pb.Empty, error) {
	f.pathCalls = append(f.pathCalls, [2]string{r.GetFromVersion(), r.GetToVersion()})
	if f.pathErr != nil {
		return nil, f.pathErr
	}
	return &pb.Empty{}, nil
}

func connectTestProvider(t *testing.T, srv pb.RuntimeServer) *runtimegrpc.Provider {
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

func TestAttachEngine_upgrade宣言ありならCheckUpgradePathをプロバイダに配線する(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	c.Pool = "tank"
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	const engine = "attachup"
	mod := &fakeUpgradeModule{
		spec: &pb.EngineSpec{
			Engine:       engine,
			DisplayName:  "AttachUp",
			Protocols:    []*pb.ProtocolPort{{Name: "sql", Primary: true}},
			Capabilities: &pb.Capabilities{Upgrade: true},
		},
	}
	p := connectTestProvider(t, mod)
	m := domain.RuntimeManifest{
		Engine:       engine,
		DisplayName:  "AttachUp",
		Protocols:    []domain.ManifestPort{{Name: "sql", Primary: true}},
		Capabilities: map[string]bool{"upgrade": true},
	}
	if err := attachEngine(a, c, m, p); err != nil {
		t.Fatalf("attachEngine: %v", err)
	}

	from := domain.RuntimeRef{Engine: engine, Version: "1.0"}
	to := domain.RuntimeRef{Engine: engine, Version: "2.0"}
	if err := domain.DefaultRuntimes.CheckUpgradePath(from, to); err != nil {
		t.Fatalf("CheckUpgradePath allowed path: %v", err)
	}
	if len(mod.pathCalls) != 1 || mod.pathCalls[0] != [2]string{"1.0", "2.0"} {
		t.Fatalf("pathCalls = %v", mod.pathCalls)
	}

	mod.pathErr = status.Error(codes.FailedPrecondition, "major jump forbidden")
	err = domain.DefaultRuntimes.CheckUpgradePath(from, to)
	if err == nil || !strings.Contains(err.Error(), "major jump forbidden") {
		t.Fatalf("CheckUpgradePath refused path err = %v", err)
	}

	svc, err := a.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Servers[domain.Engine(engine)].(domain.Upgrader); !ok {
		t.Fatalf("attached DBServer does not implement domain.Upgrader")
	}
}

func TestAttachEngine_upgrade宣言なしならCheckUpgradePathを設定しない(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	const engine = "attachnoup"
	mod := &fakeUpgradeModule{
		spec: &pb.EngineSpec{
			Engine:       engine,
			DisplayName:  "AttachNoUp",
			Protocols:    []*pb.ProtocolPort{{Name: "sql", Primary: true}},
			Capabilities: &pb.Capabilities{Query: true},
		},
		pathErr: status.Error(codes.Internal, "must not be called"),
	}
	p := connectTestProvider(t, mod)
	m := domain.RuntimeManifest{
		Engine:       engine,
		DisplayName:  "AttachNoUp",
		Protocols:    []domain.ManifestPort{{Name: "sql", Primary: true}},
		Capabilities: map[string]bool{"query": true},
	}
	if err := attachEngine(a, c, m, p); err != nil {
		t.Fatalf("attachEngine: %v", err)
	}
	spec, ok := domain.DefaultRuntimes.Spec(domain.Engine(engine))
	if !ok {
		t.Fatalf("engine %s not registered", engine)
	}
	if spec.CheckUpgradePath != nil {
		t.Fatal("CheckUpgradePath must be nil when upgrade capability is false")
	}
	from := domain.RuntimeRef{Engine: engine, Version: "1.0"}
	to := domain.RuntimeRef{Engine: engine, Version: "2.0"}
	if err := domain.DefaultRuntimes.CheckUpgradePath(from, to); err != nil {
		t.Fatalf("CheckUpgradePath without engine validator: %v", err)
	}
	if len(mod.pathCalls) != 0 {
		t.Fatalf("provider CheckUpgradePath must not be called: %v", mod.pathCalls)
	}
}

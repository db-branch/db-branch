// Command fakeruntime is a reference runtime module: a database engine
// "fakedb" that is a few lines of TCP server, written to exercise the whole
// contract (docs/runtime-modules-standard.md) on a real node without a real
// database. The same binary is the module (control plane, gRPC) and, with the
// argument "serve", the database server the module's LaunchPlan starts.
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
	"github.com/db-branch/db-branch/pkg/ext/runtimeplugin"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(serve(os.Args[2:]))
	}
	runtimeplugin.Serve(module{})
}

type module struct{ pb.UnimplementedRuntimeServer }

func empty() (*pb.Empty, error) { return &pb.Empty{}, nil }

func (module) Describe(context.Context, *pb.DescribeRequest) (*pb.EngineSpec, error) {
	return &pb.EngineSpec{
		Engine: "fakedb", DisplayName: "FakeDB", RunAsUser: "db-branch-rt-fakedb", VersionPattern: `^[0-9]+\.[0-9]+$`,
		Protocols:    []*pb.ProtocolPort{{Name: "sql", Primary: true}},
		Capabilities: &pb.Capabilities{Query: true, Upgrade: true, SourceDump: true},
	}, nil
}

func (module) Catalog(context.Context, *pb.CatalogRequest) (*pb.CatalogResponse, error) {
	return &pb.CatalogResponse{}, nil
}

func (module) PrepareRuntime(context.Context, *pb.PrepareRuntimeRequest) (*pb.Empty, error) {
	return empty()
}

func (module) Initialize(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	if err := os.WriteFile(filepath.Join(r.Instance.DataDir, "FAKEDB_VERSION"), []byte("1\n"), 0o640); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return empty()
}

// PrepareClone forgets that the source was shut down cleanly.
func (module) PrepareClone(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	_ = os.Remove(filepath.Join(r.Instance.DataDir, "clean"))
	return empty()
}

func (module) PrepareStart(_ context.Context, r *pb.InstanceRequest) (*pb.LaunchPlan, error) {
	i := r.Instance
	exe, err := os.Executable()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	port := i.Ports["sql"]
	bind := i.Bind
	if i.Maintenance {
		bind = "127.0.0.1" // no network in maintenance: the fake only listens locally
	}
	conf := fmt.Sprintf("port=%d\nbind=%s\nversion=%s\nruntime_dir=%s\n", port, bind, i.Version, i.RuntimeDir)
	if err := os.WriteFile(filepath.Join(i.StateDir, "fakedb.conf"), []byte(conf), 0o640); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.LaunchPlan{
		Command: exe, Args: []string{"serve", "--bind", bind, "--port", strconv.Itoa(int(port)), "--data", i.DataDir},
		ServiceType: "simple", KillSignal: "SIGTERM", TimeoutStopSec: 30,
	}, nil
}

func (module) Health(_ context.Context, r *pb.InstanceRequest) (*pb.HealthResponse, error) {
	addr := net.JoinHostPort(r.Instance.Bind, strconv.Itoa(int(r.Instance.Ports["sql"])))
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return &pb.HealthResponse{State: pb.HealthState_HEALTH_STATE_STOPPED}, nil
	}
	_ = c.Close()
	return &pb.HealthResponse{State: pb.HealthState_HEALTH_STATE_READY}, nil
}

func (module) Quiesce(context.Context, *pb.QuiesceRequest) (*pb.Empty, error) { return empty() }

func (module) VerifyCleanShutdown(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	if _, err := os.Stat(filepath.Join(r.Instance.DataDir, "clean")); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "no clean marker: the server was killed")
	}
	return empty()
}

func (module) RemoveInstance(_ context.Context, r *pb.InstanceRequest) (*pb.Empty, error) {
	_ = os.Remove(filepath.Join(r.Instance.StateDir, "fakedb.conf"))
	_ = os.Remove(filepath.Join(r.Instance.StateDir, "users"))
	_ = os.Remove(filepath.Join(r.Instance.StateDir, "loaded"))
	return empty()
}

func (module) CreateUser(_ context.Context, r *pb.CreateUserRequest) (*pb.Empty, error) {
	f, err := os.OpenFile(filepath.Join(r.Instance.StateDir, "users"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", r.User)
	return empty()
}

func (module) ExecAs(_ context.Context, r *pb.ExecAsRequest) (*pb.Empty, error) {
	f, err := os.OpenFile(filepath.Join(r.Instance.DataDir, "statements"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer f.Close()
	fmt.Fprintf(f, "%s: %s\n", r.User, strings.ReplaceAll(r.Sql, "\n", " "))
	return empty()
}

func (module) AdminAccess(_ context.Context, r *pb.InstanceRequest) (*pb.AdminAccessInfo, error) {
	return &pb.AdminAccessInfo{Engine: "fakedb", Host: r.Instance.Bind, Port: r.Instance.Ports["sql"], User: "admin"}, nil
}

func (module) FormatDSN(_ context.Context, r *pb.FormatDSNRequest) (*pb.FormatDSNResponse, error) {
	port := r.Instance.Ports["sql"]
	return &pb.FormatDSNResponse{Dsn: fmt.Sprintf("fakedb://%s:%s@%s:%d/", r.User, r.Password, r.Host, port)}, nil
}

func (module) Query(_ context.Context, r *pb.QueryRequest) (*pb.QueryResponse, error) {
	return &pb.QueryResponse{Columns: []string{"answer"}, Rows: []*pb.Row{{Cells: []*pb.Cell{{Value: "42"}}}}}, nil
}

func (module) TailLog(context.Context, *pb.TailLogRequest) (*pb.TailLogResponse, error) {
	return &pb.TailLogResponse{Text: "fakedb: nothing to say\n"}, nil
}

func (module) DumpSource(_ context.Context, r *pb.DumpSourceRequest) (*pb.Empty, error) {
	outPath := r.GetOutPath()
	if outPath == "" {
		return nil, status.Error(codes.InvalidArgument, "out_path is required")
	}
	fi, err := os.Stat(filepath.Dir(outPath))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var parentUID uint32
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		parentUID = st.Uid
	}
	body := fmt.Sprintf("-- fakedb dump\nuid=%d\nparent_uid=%d\n", os.Geteuid(), parentUID)
	if err := os.WriteFile(outPath, []byte(body), 0o600); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return empty()
}

func (module) Load(_ context.Context, r *pb.LoadRequest) (*pb.Empty, error) {
	b, err := os.ReadFile(r.GetDumpPath())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	stateDir := r.GetInstance().GetStateDir()
	if stateDir == "" {
		return nil, status.Error(codes.InvalidArgument, "state_dir is required")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "loaded"), b, 0o640); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return empty()
}

func (module) CheckUpgradePath(_ context.Context, r *pb.CheckUpgradePathRequest) (*pb.Empty, error) {
	from, err := parseVersion(r.GetFromVersion())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	to, err := parseVersion(r.GetToVersion())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if compareVersions(from, to) >= 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot upgrade from %s to %s: target must be newer", r.GetFromVersion(), r.GetToVersion())
	}
	return empty()
}

func (module) CheckUpgrade(context.Context, *pb.CheckUpgradeRequest) (*pb.UpgradePlan, error) {
	return &pb.UpgradePlan{}, nil
}

func (m module) StartUpgraded(ctx context.Context, r *pb.StartUpgradedRequest) (*pb.StartUpgradedResponse, error) {
	plan, err := m.PrepareStart(ctx, &pb.InstanceRequest{Instance: r.GetInstance()})
	if err != nil {
		return nil, err
	}
	return &pb.StartUpgradedResponse{Plan: plan}, nil
}

func parseVersion(s string) ([]int, error) {
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid version %q", s)
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid version %q", s)
		}
		out[i] = n
	}
	return out, nil
}

func compareVersions(a, b []int) int {
	n := max(len(a), len(b))
	for i := 0; i < n; i++ {
		var va, vb int
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// serve is the database server: it answers every line with "OK <line>" and,
// on SIGTERM, leaves a marker saying it shut down cleanly.
func serve(args []string) int {
	var bind, port, data string
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--bind":
			bind = args[i+1]
		case "--port":
			port = args[i+1]
		case "--data":
			data = args[i+1]
		}
	}
	_ = os.Remove(filepath.Join(data, "clean"))
	l, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakedb:", err)
		return 1
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		_ = os.WriteFile(filepath.Join(data, "clean"), []byte("1\n"), 0o640)
		os.Exit(0)
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			return 1
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				fmt.Fprintf(c, "OK %s\n", sc.Text())
			}
		}()
	}
}

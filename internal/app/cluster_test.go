package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/interface/cli"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
)

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustName(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestImportSingleNodeState_SettingsTokensModulesRefreshesおよびBaseAnonymousを全移行しRecover前に完了する(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	// Seed a single-node state.json with every kind of state.
	statePath := filepath.Join(cfg.DataDir, "state.json")
	old := &filestore.Store{Path: statePath}
	if err := old.PutBase(ctx, domain.Base{Name: mustName("prod"), Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4"}, Status: domain.BaseReady}); err != nil {
		t.Fatal(err)
	}
	maxAnon := 3
	if err := old.SetBaseAnonymous(ctx, mustName("prod"), domain.AnonymousLimits{MaxBranches: &maxAnon, MaxTTL: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := old.SetBaseReplicaNodes(ctx, mustName("prod"), &[]string{"node-b"}); err != nil {
		t.Fatal(err)
	}
	// Self-node replica entry must be skipped; non-self replica must be preserved.
	if err := old.AddReplica(ctx, mustName("prod"), "s1", "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := old.AddReplica(ctx, mustName("prod"), "s1", "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := old.PutBranch(ctx, domain.Branch{Name: mustName("dev"), Base: mustName("prod"), Status: domain.BranchReady, Record: true}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutRouteUser(ctx, domain.RouteUser{Name: "app", DefaultBranch: mustName("dev")}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "1.0.0", Wanted: true}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutSettings(ctx, "core", map[string]string{"branch.memory_mb": "512", "branch.open_password": "secret123"}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutSettings(ctx, "audit", map[string]string{"retain_days": "90"}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutSettings(ctx, "pref:admin", map[string]string{"language": "ja"}); err != nil {
		t.Fatal(err)
	}
	trialAt := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if _, err := old.StartTrial(ctx, trialAt); err != nil {
		t.Fatal(err)
	}
	if err := old.PutToken(ctx, domain.APIToken{ID: "tok-1", Name: "ci", Hash: "deadbeef", Scope: domain.ScopeBranches, CreatedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutModuleData(ctx, "auth", "user/alice", []byte(`{"role":"admin"}`)); err != nil {
		t.Fatal(err)
	}
	if err := old.SetBaseRefresh(ctx, domain.BaseRefresh{Base: mustName("prod"), Every: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := old.RecordBaseRefreshRun(ctx, mustName("prod"), domain.RefreshRun{StartedAt: time.Unix(200, 0).UTC(), FinishedAt: time.Unix(210, 0).UTC(), OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutUpdateCheck(ctx, domain.UpdateCheck{At: time.Unix(300, 0).UTC(), By: "node-a"}); err != nil {
		t.Fatal(err)
	}
	ro := domain.NewRollout("r1", "1.0.0", []string{"node-a"}, map[string]string{"node-a": "0.2.7"}, time.Unix(400, 0).UTC())
	if err := old.StartRollout(ctx, ro); err != nil {
		t.Fatal(err)
	}
	if _, err := old.InitClusterID(ctx, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}

	// Prepare a bootstrapped one-node cluster config with ephemeral ports.
	dir := clusterDir(cfg)
	ca, err := clusterpki.InitCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dir, "node"), "node-a", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cc := ClusterConfig{
		Node:      "node-a",
		APIAddr:   freeTCPAddr(t),
		RaftAddr:  freeTCPAddr(t),
		Bootstrap: true,
	}
	if err := saveClusterConfig(cfg, cc); err != nil {
		t.Fatal(err)
	}

	cl, err := startCluster(ctx, cfg, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.close()

	if err := cl.waitLeader(ctx); err != nil {
		t.Fatalf("leader not elected: %v", err)
	}

	// Pre-populate a newer cluster setting to verify non-destructive merge on retry.
	if err := cl.store.PutSettings(ctx, "core", map[string]string{"branch.memory_mb": "1024"}); err != nil {
		t.Fatal(err)
	}

	if err := cl.importSingleNodeState(ctx, discardLog()); err != nil {
		t.Fatalf("importSingleNodeState: %v", err)
	}

	// Verify state.json was moved to state.json.imported.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state.json should be moved aside after import, stat err = %v", err)
	}
	if _, err := os.Stat(statePath + ".imported"); err != nil {
		t.Errorf("state.json.imported should exist: %v", err)
	}

	// Verify EVERY migrated entity is present immediately.
	base, err := cl.store.GetBase(ctx, mustName("prod"))
	if err != nil {
		t.Fatalf("GetBase: %v", err)
	}
	if base.Node != "node-a" {
		t.Errorf("base.Node = %q, want node-a", base.Node)
	}
	if base.Anonymous.MaxBranches == nil || *base.Anonymous.MaxBranches != 3 || base.Anonymous.MaxTTL != 2*time.Hour {
		t.Errorf("base.Anonymous = %+v", base.Anonymous)
	}
	if base.ReplicaNodes == nil || len(*base.ReplicaNodes) != 1 || (*base.ReplicaNodes)[0] != "node-b" {
		t.Errorf("base.ReplicaNodes = %+v", base.ReplicaNodes)
	}
	// Self-node must NOT be duplicated in Replicas; only node-b is recorded.
	if base.HasReplica("s1", "node-a") || !base.HasReplica("s1", "node-b") {
		t.Errorf("base.Replicas = %+v, want only node-b", base.Replicas)
	}

	br, err := cl.store.GetBranch(ctx, mustName("dev"))
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if br.Node != "node-a" || !br.Record {
		t.Errorf("branch = %+v", br)
	}

	ru, _ := cl.store.ListRouteUsers(ctx)
	if len(ru) != 1 || ru[0].Name != "app" {
		t.Errorf("route users = %+v", ru)
	}
	mods, _ := cl.store.ListModules(ctx)
	if len(mods) != 1 || mods[0].Name != "audit" {
		t.Errorf("modules = %+v", mods)
	}
	// Existing Raft setting ("1024") must win over state.json ("512"), while missing keys ("secret123") are imported.
	coreSet, _ := cl.store.GetSettings(ctx, "core")
	if coreSet["branch.memory_mb"] != "1024" || coreSet["branch.open_password"] != "secret123" {
		t.Errorf("core settings = %+v", coreSet)
	}
	auditSet, _ := cl.store.GetSettings(ctx, "audit")
	if auditSet["retain_days"] != "90" {
		t.Errorf("audit settings = %+v", auditSet)
	}
	prefSet, _ := cl.store.GetSettings(ctx, "pref:admin")
	if prefSet["language"] != "ja" {
		t.Errorf("pref:admin = %+v", prefSet)
	}
	if gotTrial, _ := cl.store.TrialStarted(ctx); !gotTrial.Equal(trialAt) {
		t.Errorf("trial = %v, want %v", gotTrial, trialAt)
	}
	toks, _ := cl.store.ListTokens(ctx)
	if len(toks) != 1 || toks[0].ID != "tok-1" || toks[0].Hash != "deadbeef" {
		t.Errorf("tokens = %+v", toks)
	}
	md, _ := cl.store.ListModuleData(ctx, "auth")
	if string(md["user/alice"]) != `{"role":"admin"}` {
		t.Errorf("module data = %+v", md)
	}
	ref, err := cl.store.GetBaseRefresh(ctx, mustName("prod"))
	if err != nil || ref.Every != 24*time.Hour || ref.LastRun == nil || !ref.LastRun.OK {
		t.Errorf("refresh = %+v, err = %v", ref, err)
	}
	upd, _ := cl.store.GetUpdateCheck(ctx)
	if upd.At.Unix() != 300 {
		t.Errorf("update check = %+v", upd)
	}
	gotRo, err := cl.store.GetRollout(ctx)
	if err != nil || gotRo.ID != "r1" {
		t.Errorf("rollout = %+v, err = %v", gotRo, err)
	}
	if cid := cl.store.ClusterID(ctx); cid != "0123456789abcdef0123456789abcdef" {
		t.Errorf("cluster id = %q", cid)
	}

	// Calling importSingleNodeState a second time after state.json was moved is a no-op.
	if err := cl.importSingleNodeState(ctx, discardLog()); err != nil {
		t.Errorf("second importSingleNodeState: %v", err)
	}
}

func TestRunAgent_クラスタ初回起動時にstateJSONの移行がAPI受付前に完了する(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	// Short socket path under /tmp to stay within Darwin's 104-byte sun_path limit.
	sockDir, err := os.MkdirTemp("/tmp", "dbb-w16-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	cfg.Socket = filepath.Join(sockDir, "agent.sock")
	cfg.Console.Listen = "127.0.0.1:0"

	if err := SetPool(cfg, "tank"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.DataDir, "state.json")
	old := &filestore.Store{Path: statePath}
	// Seed a base AND a branch so cl.publish exercises srv.PublishBranch (tests C-2 race).
	if err := old.PutBase(ctx, domain.Base{Name: mustName("shop"), Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4"}, Status: domain.BaseReady}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutBranch(ctx, domain.Branch{Name: mustName("dev"), Base: mustName("shop"), Status: domain.BranchFailed}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutSettings(ctx, "core", map[string]string{"branch.memory_mb": "512"}); err != nil {
		t.Fatal(err)
	}
	if err := old.PutToken(ctx, domain.APIToken{ID: "t-agent", Name: "ci", Hash: "abc", Scope: domain.ScopeBranches, CreatedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}

	dir := clusterDir(cfg)
	ca, err := clusterpki.InitCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dir, "node"), "node-a", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := saveClusterConfig(cfg, ClusterConfig{Node: "node-a", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), Bootstrap: true}); err != nil {
		t.Fatal(err)
	}

	agentCtx, stopAgent := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunAgent(agentCtx, cfg, AgentOptions{Version: "test", Log: discardLog()})
	}()

	// Wait until the socket appears; by that moment state.json must ALREADY be imported and moved aside.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(cfg.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stopAgent()
			t.Fatalf("socket did not appear; RunAgent err = %v", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state.json still present when API socket opened: %v", err)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := cli.NewSocketClient(cfg.Socket).Do(ctx, "GET", "/v1/bases", nil, &resp); err != nil {
		t.Fatalf("GET /v1/bases: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0]["name"] != "shop" {
		t.Errorf("bases immediately after start = %+v, want [shop]", resp.Items)
	}

	stopAgent()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunAgent = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not stop")
	}
}

func TestStartCluster_join未完了再起動でもraft構成空ならjoinAsMemberを再試行し同名ノード参加を拒否する(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Start node-a (bootstrapped leader).
	cfgA := config.Default()
	cfgA.DataDir = t.TempDir()
	dirA := clusterDir(cfgA)
	ca, err := clusterpki.InitCA(filepath.Join(dirA, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dirA, "node"), "node-a", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ccA := ClusterConfig{Node: "node-a", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), Bootstrap: true}
	if err := saveClusterConfig(cfgA, ccA); err != nil {
		t.Fatal(err)
	}
	clA, err := startCluster(ctx, cfgA, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	defer clA.close()
	if err := clA.waitLeader(ctx); err != nil {
		t.Fatalf("node-a did not become leader: %v", err)
	}
	if err := clA.store.PutNode(ctx, domain.Node{Name: "node-a", Address: ccA.APIAddr, RaftAddress: ccA.RaftAddr}); err != nil {
		t.Fatal(err)
	}

	// Prepare node-b pointing initially at an unreachable JoinVia address.
	cfgB := config.Default()
	cfgB.DataDir = t.TempDir()
	dirB := clusterDir(cfgB)
	if err := ca.Save(filepath.Join(dirB, "ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dirB, "node"), "node-b", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ccB := ClusterConfig{Node: "node-b", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), JoinVia: freeTCPAddr(t)}
	if err := saveClusterConfig(cfgB, ccB); err != nil {
		t.Fatal(err)
	}

	// First start: creates raft.db, but joinAsMember fails because JoinVia is unreachable.
	ctx1, cancel1 := context.WithCancel(ctx)
	clB1, err := startCluster(ctx1, cfgB, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel1()
	clB1.close()

	if !raftstore.HasState(filepath.Join(dirB, "raft")) {
		t.Fatal("expected raft.db to exist after first start")
	}

	// Second start: JoinVia is now reachable. Even though raft.db exists,
	// Raft config is empty so startCluster must retry joinAsMember.
	ccB.JoinVia = ccA.APIAddr
	if err := saveClusterConfig(cfgB, ccB); err != nil {
		t.Fatal(err)
	}
	clB2, err := startCluster(ctx, cfgB, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	defer clB2.close()

	deadline := time.Now().Add(10 * time.Second)
	for {
		srvs, _ := clB2.store.Servers(ctx)
		if len(srvs) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node-b did not join after restart: servers = %+v", srvs)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Duplicate node name via AddMember (different RaftAddr) must be rejected.
	certDup, err := ca.IssueNode(t.TempDir(), "node-b", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	err = nodeapi.NewClient(certDup.ClientTLS()).AddMember(ctx, ccA.APIAddr, "node-b", freeTCPAddr(t))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate node AddMember: err = %v, want ErrAlreadyExists", err)
	}

	// Duplicate node starting up (e.g. bypassing ClusterJoin against an older peer):
	// joinAsMember must stop on ErrAlreadyExists and unblock waitLeader with that error.
	cfgC := config.Default()
	cfgC.DataDir = t.TempDir()
	dirC := clusterDir(cfgC)
	_ = ca.Save(filepath.Join(dirC, "ca"))
	_, _ = ca.IssueNode(filepath.Join(dirC, "node"), "node-b", []string{"127.0.0.1"})
	_ = saveClusterConfig(cfgC, ClusterConfig{Node: "node-b", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), JoinVia: ccA.APIAddr})
	clC, err := startCluster(ctx, cfgC, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	defer clC.close()
	if err := clC.waitLeader(ctx); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("waitLeader on duplicate joiner: err = %v, want ErrAlreadyExists", err)
	}

	// Duplicate node name during ClusterJoin must be rejected before writing cluster.json.
	tok, err := clA.NewToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := clA.store.PutNode(ctx, domain.Node{Name: hostname(), Address: "127.0.0.1:7070"}); err != nil {
		t.Fatal(err)
	}
	cfgDup := config.Default()
	cfgDup.DataDir = t.TempDir()
	var notes strings.Builder
	err = (Node{Notes: &notes, Restart: func(context.Context) error { return nil }}).ClusterJoin(ctx, cfgDup, ccA.APIAddr, tok)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("ClusterJoin duplicate hostname: err = %v, want ErrAlreadyExists", err)
	}
	if cc, _ := LoadClusterConfig(cfgDup); cc != nil {
		t.Errorf("cluster.json should not be created on rejected duplicate join: %+v", cc)
	}

	// ClusterJoin with corrupt node.json must fail without restarting the agent.
	_ = clA.store.DeleteNode(ctx, hostname())
	cfgBadNode := config.Default()
	cfgBadNode.DataDir = t.TempDir()
	_ = os.WriteFile(filepath.Join(cfgBadNode.DataDir, "node.json"), []byte("{"), 0o600)
	restarts := 0
	err = (Node{Notes: &notes, Restart: func(context.Context) error { restarts++; return nil }}).ClusterJoin(ctx, cfgBadNode, ccA.APIAddr, tok)
	if err == nil {
		t.Error("ClusterJoin with corrupt node.json: want error")
	}
	if restarts != 0 {
		t.Errorf("ClusterJoin restarts = %d, want 0 when loadNode failed", restarts)
	}
}

func TestRunAgent_重複ノード名でjoin拒否されたらハングせず終了する(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Start leader node-a.
	cfgA := config.Default()
	cfgA.DataDir = t.TempDir()
	dirA := clusterDir(cfgA)
	ca, err := clusterpki.InitCA(filepath.Join(dirA, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dirA, "node"), "node-a", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ccA := ClusterConfig{Node: "node-a", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), Bootstrap: true}
	if err := saveClusterConfig(cfgA, ccA); err != nil {
		t.Fatal(err)
	}
	clA, err := startCluster(ctx, cfgA, "", nil, nil, nil, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	defer clA.close()
	if err := clA.waitLeader(ctx); err != nil {
		t.Fatal(err)
	}

	// Start a second RunAgent configured with the SAME node name "node-a" and different RaftAddr.
	// Both runtimeLoop and RunAgent call waitLeader; broadcast close must unblock RunAgent cleanly.
	cfgDup := config.Default()
	cfgDup.DataDir = t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "dbb-dup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	cfgDup.Socket = filepath.Join(sockDir, "agent.sock")
	cfgDup.Console.Listen = "127.0.0.1:0"
	dirDup := clusterDir(cfgDup)
	if err := ca.Save(filepath.Join(dirDup, "ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNode(filepath.Join(dirDup, "node"), "node-a", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := saveClusterConfig(cfgDup, ClusterConfig{Node: "node-a", APIAddr: freeTCPAddr(t), RaftAddr: freeTCPAddr(t), JoinVia: ccA.APIAddr}); err != nil {
		t.Fatal(err)
	}

	err = RunAgent(ctx, cfgDup, AgentOptions{Version: "test", Log: discardLog()})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("RunAgent on duplicate joiner: err = %v, want ErrAlreadyExists", err)
	}
}

func TestListenAddr_ループバックと非ループバックと不正ポートを正しく扱う(t *testing.T) {
	cases := []struct {
		addr string
		def  int
		want string
	}{
		{"192.168.1.10:7070", 7070, ":7070"},
		{"127.0.0.1:17070", 7070, "127.0.0.1:17070"},
		{"[::1]:17071", 7071, "[::1]:17071"},
		{"127.0.0.1:0", 7070, ":7070"},
		{"127.0.0.1:99999", 7070, ":7070"},
		{"invalid", 7071, ":7071"},
	}
	for _, tc := range cases {
		if got := listenAddr(tc.addr, tc.def); got != tc.want {
			t.Errorf("listenAddr(%q, %d) = %q, want %q", tc.addr, tc.def, got, tc.want)
		}
	}
}

func TestImportSingleNodeState_壊れたstateJSONは退避せずエラーにする(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	statePath := filepath.Join(cfg.DataDir, "state.json")
	if err := os.WriteFile(statePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &clusterRuntime{cfg: cfg}
	if err := c.importSingleNodeState(context.Background(), discardLog()); err == nil {
		t.Fatal("corrupt state.json: want error")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state.json should remain intact on error: %v", err)
	}
}

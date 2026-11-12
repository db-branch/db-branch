package app_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

func TestRunAgent_ソケットでAPIを出し止める指示で終わる(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "run", "agent.sock")
	c.Console.Listen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunAgent(ctx, c, app.AgentOptions{Version: "test"}) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(c.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := os.Stat(c.Socket)
	if info.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v", info.Mode().Perm())
	}
	var caps map[string]any
	if err := cli.NewSocketClient(c.Socket).Do(ctx, "GET", "/v1/capabilities", nil, &caps); err != nil || caps["version"] != "test" {
		t.Fatalf("capabilities = %v, %v", caps, err)
	}
	var list map[string]any
	err := cli.NewSocketClient(c.Socket).Do(ctx, "GET", "/v1/branches", nil, &list)
	if apiErr, ok := err.(*cli.APIError); !ok || apiErr.Code != "not_initialized" {
		t.Errorf("before init: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunAgent = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not stop")
	}
}

func TestRunAgent_待ち受けられなければエラー(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c.Console.Listen = l.Addr().String()
	if err := app.RunAgent(context.Background(), c, app.AgentOptions{}); err == nil {
		t.Error("port in use: want error")
	}
	c.Console.Listen = "127.0.0.1:0"
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	c.Socket = filepath.Join(file, "agent.sock")
	if err := app.RunAgent(context.Background(), c, app.AgentOptions{}); err == nil {
		t.Error("socket under a file: want error")
	}
}

func TestAdvertiseHost_設定がなければループバック以外のIPv4(t *testing.T) {
	c := cfg(t)
	c.Advertise = "db.example"
	if got := app.AdvertiseHost(c); got != "db.example" {
		t.Errorf("configured = %q", got)
	}
	c.Advertise = ""
	got := app.AdvertiseHost(c)
	if ip := net.ParseIP(got); ip == nil || ip.IsLoopback() && got != "127.0.0.1" {
		t.Errorf("detected = %q", got)
	}
}

func TestRunAgent_初期化済みなら復旧と期限切れの見回りをする(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "agent.sock")
	c.Console.Listen = "127.0.0.1:0"
	if err := app.SetPool(c, "tank"); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := app.RunAgent(ctx, c, app.AgentOptions{
		Log:         slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)),
		ExpireEvery: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "agent stopped") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestSystemNode_実機の部品を組み立てる(t *testing.T) {
	n := app.SystemNode(&strings.Builder{})
	if n.Storage == nil || n.ARC == nil || n.Restart == nil || n.InitialPasswordFile != "/etc/db-branch/initial-admin-password" {
		t.Errorf("node = %+v", n)
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestOutboundIP_既定経路のアドレスを返す(t *testing.T) {
	ip := app.OutboundIP()
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
		t.Errorf("OutboundIP = %q", ip)
	}
}

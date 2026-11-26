package app_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
)

func cfg(t *testing.T) config.Config {
	c := config.Default()
	c.DataDir = t.TempDir()
	return c
}

func TestNew_プール未設定でもランタイム管理は使える(t *testing.T) {
	a, err := app.New(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.Runtimes.Arch == "" || a.Runtimes.Catalog == nil {
		t.Errorf("runtimes = %+v", a.Runtimes)
	}
	if _, err := a.Branches(); !errors.Is(err, app.ErrNotInitialized) {
		t.Errorf("Branches error = %v, want ErrNotInitialized", err)
	}
}

func TestSetPool_initで決めたプールを覚えておく(t *testing.T) {
	c := cfg(t)
	if err := app.SetPool(c, "tank"); err != nil {
		t.Fatal(err)
	}
	a, _ := app.New(c)
	svc, err := a.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if svc.Pool != "tank" || svc.Ports.Min != 40000 || svc.MemoryMB != 128 {
		t.Errorf("service = %+v", svc)
	}
}

func TestNew_設定ファイルのプールが優先される(t *testing.T) {
	c := cfg(t)
	_ = app.SetPool(c, "tank")
	c.Pool = "fast"
	a, _ := app.New(c)
	svc, _ := a.Branches()
	if svc.Pool != "fast" {
		t.Errorf("pool = %q", svc.Pool)
	}
}

func TestNew_壊れたノードファイルはエラーにする(t *testing.T) {
	c := cfg(t)
	_ = os.WriteFile(filepath.Join(c.DataDir, "node.json"), []byte("{"), 0o600)
	if _, err := app.New(c); err == nil {
		t.Fatal("want error")
	}
}

func TestSetPool_書けなければエラーにする(t *testing.T) {
	c := cfg(t)
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	c.DataDir = filepath.Join(file, "sub")
	if err := app.SetPool(c, "tank"); err == nil {
		t.Fatal("want error")
	}
}

func TestPassword_英数字24文字の推測できない値を作る(t *testing.T) {
	a, b := app.Password(), app.Password()
	if !regexp.MustCompile(`^[A-Za-z0-9]{24}$`).MatchString(a) || a == b {
		t.Errorf("passwords %q %q", a, b)
	}
}

func TestChownTo_存在しないユーザーならエラーにする(t *testing.T) {
	if err := app.ChownTo("no-such-user-dbb")(t.TempDir()); err == nil {
		t.Fatal("want error")
	}
}

func TestChownTo_自分自身には渡せる(t *testing.T) {
	u := os.Getenv("USER")
	if u == "" {
		t.Skip("USER not set")
	}
	if err := app.ChownTo(u)(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestNew_ノードファイルが読めなければエラーにする(t *testing.T) {
	c := cfg(t)
	_ = os.MkdirAll(filepath.Join(c.DataDir, "node.json", "x"), 0o755)
	if _, err := app.New(c); err == nil {
		t.Fatal("want error")
	}
}

func TestNew_アーティファクト取得クライアントはDefaultClientではなくタイムアウトを持つ(t *testing.T) {
	a, err := app.New(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	cache, ok := a.Runtimes.Cache.(artifact.Cache)
	if !ok {
		t.Fatalf("unexpected Cache type: %T", a.Runtimes.Cache)
	}
	hc := cache.Client
	if hc == nil || hc == http.DefaultClient {
		t.Fatalf("artifact HTTP client must not be nil or http.DefaultClient: %v", hc)
	}
	if hc.Timeout <= 0 || hc.Transport == nil {
		t.Fatalf("artifact HTTP client missing timeout or transport: Timeout=%v Transport=%v", hc.Timeout, hc.Transport)
	}
}

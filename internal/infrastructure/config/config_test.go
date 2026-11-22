package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_設定ファイルがなければ既定値で動く(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != config.Default() {
		t.Errorf("got %+v, want defaults %+v", got, config.Default())
	}
}

func TestDefault_FHSに沿った場所を使う(t *testing.T) {
	d := config.Default()
	if d.DataDir != "/var/lib/db-branch" {
		t.Errorf("DataDir = %q", d.DataDir)
	}
	if d.Socket != "/run/db-branch/agent.sock" {
		t.Errorf("Socket = %q", d.Socket)
	}
	if d.Console.Listen != ":7080" {
		t.Errorf("Console.Listen = %q", d.Console.Listen)
	}
}

func TestLoad_書いた項目だけが上書きされ残りは既定値のまま(t *testing.T) {
	path := writeFile(t, "pool = \"tank\"\n\n[console]\nlisten = \"127.0.0.1:9000\"\n")

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Pool != "tank" {
		t.Errorf("Pool = %q, want tank", got.Pool)
	}
	if got.Console.Listen != "127.0.0.1:9000" {
		t.Errorf("Console.Listen = %q", got.Console.Listen)
	}
	if got.DataDir != config.Default().DataDir {
		t.Errorf("DataDir = %q, want default", got.DataDir)
	}
}

func TestLoad_知らない項目はタイプミスとしてエラーにする(t *testing.T) {
	path := writeFile(t, "pol = \"tank\"\n")

	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "pol") {
		t.Fatalf("want error mentioning unknown key 'pol', got %v", err)
	}
}

func TestLoad_壊れたTOMLはエラーにする(t *testing.T) {
	path := writeFile(t, "pool = \n")

	if _, err := config.Load(path); err == nil {
		t.Fatal("want parse error, got nil")
	}
}

func TestLoad_読めないファイルはエラーにする(t *testing.T) {
	if _, err := config.Load(t.TempDir()); err == nil {
		t.Fatal("want error when path is a directory, got nil")
	}
}

func TestDefault_ブランチ用のポート範囲とメモリと待ち受けアドレス(t *testing.T) {
	d := config.Default()
	if d.Ports.Min != 40000 || d.Ports.Max != 40999 {
		t.Errorf("Ports = %+v", d.Ports)
	}
	if d.Branch.MemoryMB != 128 {
		t.Errorf("Branch.MemoryMB = %d", d.Branch.MemoryMB)
	}
	if d.Bind != "0.0.0.0" {
		t.Errorf("Bind = %q", d.Bind)
	}
}

func TestLoad_ポート範囲が逆転していればエラーにする(t *testing.T) {
	path := writeFile(t, "[ports]\nmin = 50000\nmax = 40000\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_署名のないランタイムモジュールの許可は既定で無効で設定で有効にできる(t *testing.T) {
	got, err := config.Load(writeFile(t, "pool = \"tank\"\n"))
	if err != nil || got.AllowUnsignedRuntimes {
		t.Errorf("default: %+v, %v", got.AllowUnsignedRuntimes, err)
	}
	got, err = config.Load(writeFile(t, "allow_unsigned_runtimes = true\n"))
	if err != nil || !got.AllowUnsignedRuntimes {
		t.Errorf("enabled: %+v, %v", got.AllowUnsignedRuntimes, err)
	}
	if _, err := config.Load(writeFile(t, "allow_unsigned_runtimes = \"yes\"\n")); err == nil {
		t.Error("a string where a boolean is needed was accepted")
	}
}

func TestLoad_通知のプライベートIP許可は既定で無効で設定で有効にできる(t *testing.T) {
	got, err := config.Load(writeFile(t, "pool = \"tank\"\n"))
	if err != nil || got.Notify.AllowPrivateIPs {
		t.Errorf("default: %+v, %v", got.Notify.AllowPrivateIPs, err)
	}
	got, err = config.Load(writeFile(t, "[notify]\nallow_private_ips = true\n"))
	if err != nil || !got.Notify.AllowPrivateIPs {
		t.Errorf("enabled: %+v, %v", got.Notify.AllowPrivateIPs, err)
	}
	if _, err := config.Load(writeFile(t, "[notify]\nallow_private_ips = \"yes\"\n")); err == nil {
		t.Error("a string where a boolean is needed was accepted")
	}
}

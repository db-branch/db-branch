package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
)

func TestServiceIDHost_nodeJSONからPoolを解決してsvcidsHostに渡す(t *testing.T) {
	t.Run("node.jsonのpoolを使う", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		if err := SetPool(cfg, "tank"); err != nil {
			t.Fatal(err)
		}
		h, err := serviceIDHost(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pool != "tank" {
			t.Errorf("Pool = %q, want %q", h.Pool, "tank")
		}
	})

	t.Run("config.tomlのpoolが優先される", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		if err := SetPool(cfg, "tank"); err != nil {
			t.Fatal(err)
		}
		cfg.Pool = "fast"
		h, err := serviceIDHost(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pool != "fast" {
			t.Errorf("Pool = %q, want %q", h.Pool, "fast")
		}
	})

	t.Run("モジュールストレージのときはZFSのPoolを空にする", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		cfg.Storage = "fsx"
		if err := SetPool(cfg, "fsx"); err != nil {
			t.Fatal(err)
		}
		cfg.Storage = ""
		h, err := serviceIDHost(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pool != "" {
			t.Errorf("Pool = %q, want empty for module storage", h.Pool)
		}
	})

	t.Run("config.tomlのstorage指定がnode.jsonのpoolを上書きして空にする", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		if err := SetPool(cfg, "tank"); err != nil {
			t.Fatal(err)
		}
		cfg.Storage = "fsx"
		h, err := serviceIDHost(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pool != "" {
			t.Errorf("Pool = %q, want empty when config.Storage is set", h.Pool)
		}
	})

	t.Run("node.jsonがなければ空Poolで成功する", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		h, err := serviceIDHost(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if h.Pool != "" {
			t.Errorf("Pool = %q, want empty", h.Pool)
		}
	})

	t.Run("壊れたnode.jsonはエラーにしAlignIDsは再起動しない", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "node.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := serviceIDHost(cfg); err == nil {
			t.Error("corrupt node.json: want error")
		}
		restarts := 0
		var notes strings.Builder
		n := Node{Notes: &notes, Restart: func(context.Context) error { restarts++; return nil }}
		if _, err := n.AlignIDs(context.Background(), cfg, domain.ServiceIDs{UID: 999, GID: 999}); err == nil {
			t.Error("AlignIDs with corrupt node.json: want error")
		}
		if restarts != 0 {
			t.Errorf("restarts = %d, want 0 when nothing was stopped", restarts)
		}
	})
}

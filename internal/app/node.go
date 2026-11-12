package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/tlscert"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
	"github.com/db-branch/db-branch/internal/infrastructure/zfsarc"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// ARC caps the ZFS ARC.
type ARC interface {
	DefaultMax() (uint64, error)
	Apply(max uint64) (string, error)
}

// Node performs root-only node setup: `db-branch init` and `admin reset-password`.
type Node struct {
	Storage             domain.Storage
	ARC                 ARC
	InitialPasswordFile string
	// Restart restarts the agent so it picks up the new setup.
	Restart func(ctx context.Context) error
	// Notes receives human-readable remarks (e.g. the ARC limit).
	Notes io.Writer
}

// InitialPasswordFile holds the password `db-branch init` generated until it is reset.
const InitialPasswordFile = "/etc/db-branch/initial-admin-password"

// SystemNode is the real node.
func SystemNode(notes io.Writer) Node {
	return Node{
		Storage:             zfs.New(execx.Runner{}),
		ARC:                 zfsarc.System,
		InitialPasswordFile: InitialPasswordFile,
		Restart: func(ctx context.Context) error {
			if _, err := os.Stat("/run/systemd/system"); err != nil {
				return nil // not under systemd (containers, tests)
			}
			return exec.CommandContext(ctx, "systemctl", "restart", "db-branch.service").Run()
		},
		Notes: notes,
	}
}

// Init prepares the node on pool and returns the initial admin password, or
// "" when a password already exists. Running it again is safe.
func (n Node) Init(ctx context.Context, cfg config.Config, pool string) (string, error) {
	if cfg.Storage != "" {
		// The module's storage is set up when the agent loads it.
		if err := SetPool(cfg, pool); err != nil {
			return "", err
		}
		return n.finish(ctx, cfg)
	}
	if ok, err := n.Storage.Exists(ctx, pool); err != nil || !ok {
		return "", errors.Join(fmt.Errorf("pool %q not found (see `zpool list`)", pool), err)
	}
	if err := SetPool(cfg, pool); err != nil {
		return "", err
	}
	if err := (&dbbranch.Service{Storage: n.Storage, Pool: pool}).Init(ctx); err != nil {
		return "", err
	}
	max, err := initialArc(cfg, n.ARC)
	if err != nil {
		return "", err
	}
	note, err := n.ARC.Apply(max)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(n.Notes, note)
	return n.finish(ctx, cfg)
}

// initialArc is the ARC cap `db-branch init` sets: config.toml's, or a
// quarter of physical memory.
func initialArc(cfg config.Config, a interface{ DefaultMax() (uint64, error) }) (uint64, error) {
	if mb := cfg.ZFS.ArcMaxMB; mb > 0 {
		return mb << 20, nil
	}
	return a.DefaultMax()
}

// finish makes the admin password and TLS certificate and restarts the agent.
func (n Node) finish(ctx context.Context, cfg config.Config) (string, error) {
	password, err := n.initialPassword(cfg)
	if err != nil {
		return "", err
	}
	if _, _, err := tlscert.Ensure(filepath.Join(cfg.DataDir, "tls"), []string{hostname(), AdvertiseHost(cfg)}); err != nil {
		return "", err
	}
	return password, n.Restart(ctx)
}

func hostname() string { h, _ := os.Hostname(); return h }

func (n Node) initialPassword(cfg config.Config) (string, error) {
	svc := authService(cfg)
	if _, err := svc.Store.Load(); !errors.Is(err, auth.ErrNoCredentials) {
		return "", err
	}
	password := auth.GeneratePassword()
	if err := os.MkdirAll(filepath.Dir(n.InitialPasswordFile), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(n.InitialPasswordFile, []byte(password+"\n"), 0o600); err != nil {
		return "", err
	}
	return password, svc.SetPassword(password)
}

// ResetPassword sets a new random admin password, which ends every console
// session on the next request.
func (n Node) ResetPassword(cfg config.Config) (string, error) {
	if cc, err := LoadClusterConfig(cfg); err != nil || cc != nil {
		return "", errors.Join(errors.New("in a cluster the password is reset through the agent; start it (systemctl start db-branch) and run this again"), err)
	}
	password := auth.GeneratePassword()
	if err := authService(cfg).SetPassword(password); err != nil {
		return "", err
	}
	_ = os.Remove(n.InitialPasswordFile)
	return password, nil
}

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/arccap"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/settings"
)

func TestWireModulesAndSettings_backupDirの危険なパスを拒否する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st := &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")}
	// Pre-populate an invalid backup.dir as if upgraded from an older version.
	if err := st.PutSettings(ctx, settings.CoreScope, map[string]string{"backup.dir": "/etc"}); err != nil {
		t.Fatal(err)
	}

	svc := &dbbranch.Service{Store: st}
	srv := &httpapi.Server{Auth: &auth.Service{}}
	arc := &arcNode{ctl: &arccap.Controller{}}
	problems := &moduleProblems{m: map[string]string{}}

	backupPath.Store("")
	t.Cleanup(func() { backupPath.Store("") })

	wire := func() {
		wCtx, wCancel := context.WithCancel(ctx)
		_ = wireModulesAndSettings(wCtx, cfg, svc, srv, nil, nil, nil, nil,
			func() int { return 1 }, problems, arc, nil, nil, func() bool { return true }, discardLog())
		wCancel()
	}

	wire()

	// Pre-existing invalid setting must not be applied to backupPath.
	if got, _ := backupPath.Load().(string); got != "" {
		t.Errorf("backupPath = %q after invalid stored setting, want empty", got)
	}

	// Putting a dangerous path through settings service must be rejected.
	for _, bad := range []string{"/", "/etc", "/var/lib", "/var/run/db-branch", "/root/.ssh", "relative/path", "/var/backups/../etc"} {
		if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": bad}); !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("Put(backup.dir=%q) = %v, want ErrInvalid", bad, err)
		}
	}

	// Putting a valid path succeeds and is picked up by wireModulesAndSettings.
	valid := filepath.Join(t.TempDir(), "backups")
	if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": valid}); err != nil {
		t.Fatalf("Put valid backup.dir: %v", err)
	}
	wire()
	if got, _ := backupPath.Load().(string); got != valid {
		t.Errorf("backupPath = %q, want %q", got, valid)
	}

	// Clearing resets back to default empty.
	if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{"backup.dir": ""}); err != nil {
		t.Fatalf("Put empty backup.dir: %v", err)
	}
	wire()
	if got, _ := backupPath.Load().(string); got != "" {
		t.Errorf("backupPath after clear = %q, want empty", got)
	}
}

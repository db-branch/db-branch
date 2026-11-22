package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

func TestResourceLimits_設定値がサービスの上限に変換され既定は無制限(t *testing.T) {
	refQuotaGB.Store(0)
	maxPerUser.Store(0)
	maxBranchTTL.Store(0)
	if got := resourceLimits(); got != (dbbranch.Limits{}) {
		t.Errorf("default = %+v, want no limit", got)
	}
	refQuotaGB.Store(2)
	maxPerUser.Store(5)
	maxBranchTTL.Store(int64(48 * time.Hour))
	t.Cleanup(func() { refQuotaGB.Store(0); maxPerUser.Store(0); maxBranchTTL.Store(0) })
	want := dbbranch.Limits{RefQuotaBytes: 2 << 30, MaxBranchesPerUser: 5, MaxTTL: 48 * time.Hour}
	if got := resourceLimits(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	keys := map[string]string{}
	for _, s := range coreSettings(config.Default()) {
		keys[s.Key] = s.Default
	}
	for _, k := range []string{"branch.refquota_gb", "branch.max_per_user", "branch.max_ttl"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("setting %s is not declared", k)
		}
	}
}

package app

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/backupdir"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/infrastructure/uploads"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/notify"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

// newInt64 and newBool make the atomics that back live settings.
func newInt64(v int64) *atomic.Int64 { n := new(atomic.Int64); n.Store(v); return n }
func newBool(v bool) *atomic.Bool    { b := new(atomic.Bool); b.Store(v); return b }

// Backup settings (backup.interval, backup.keep, backup.dir).
var (
	backupEvery = newInt64(int64(6 * time.Hour))
	backupKeep  = newInt64(14)
	backupPath  atomic.Value
)

// backupDir resolves the backup directory from the setting at each use.
type backupDir struct{ cfg config.Config }

func (d backupDir) dir() backupdir.Dir {
	if p, ok := backupPath.Load().(string); ok && p != "" {
		return backupdir.Dir{Path: p}
	}
	return backupdir.Dir{Path: filepath.Join(d.cfg.DataDir, "backups")}
}

func (d backupDir) Create(name string) (io.WriteCloser, error) { return d.dir().Create(name) }
func (d backupDir) List() ([]domain.BackupFile, error)         { return d.dir().List() }
func (d backupDir) Remove(name string) error                   { return d.dir().Remove(name) }
func (d backupDir) Open(name string) (io.ReadCloser, error)    { return d.dir().Open(name) }

// UploadMaxAge is how long an unused uploaded dump is kept.
const UploadMaxAge = 24 * time.Hour

// uploadMaxBytes follows the upload.max_mb setting.
var uploadMaxBytes = newInt64(20 << 30)

func uploadStore(cfg config.Config) *uploads.Store {
	return &uploads.Store{
		Dir:      filepath.Join(cfg.DataDir, "uploads"),
		MaxBytes: uploadMaxBytes.Load,
		Reserve:  uploads.DefaultReserve,
		Free:     uploads.DiskFree,
		Clock:    time.Now,
	}
}

// Notification settings (notify.*).
var (
	notifySlack, notifyWebhook, notifySecret atomic.Value
	notifyWarn                               = newInt64(int64(time.Hour))
)

func loadString(v *atomic.Value) func() string {
	return func() string { s, _ := v.Load().(string); return s }
}

// singleNotifyState adapts a single node's branch store and operation manager
// to notify.State. Peer node health is not tracked in single-node mode.
type singleNotifyState struct {
	branches func(context.Context) ([]domain.Branch, error)
	ops      func() []operation.Operation
}

func (s singleNotifyState) ListBranches(ctx context.Context) ([]domain.Branch, error) {
	return s.branches(ctx)
}

func (s singleNotifyState) ListOperations(context.Context) ([]operation.Operation, error) {
	return s.ops(), nil
}

func (s singleNotifyState) ListNodes(context.Context) ([]domain.Node, error) {
	return nil, nil
}

// newNotifier tells Slack and a webhook about expiring branches, failed
// operations and (in cluster mode) nodes going down.
func newNotifier(st notify.State, allowPrivateIPs bool, downAfter time.Duration) *notify.Service {
	return &notify.Service{State: st, Clock: time.Now, Since: time.Now(), DownAfter: downAfter,
		Warn: func() time.Duration { return time.Duration(notifyWarn.Load()) },
		Senders: []notify.Sender{
			notifier.Slack{URL: loadString(&notifySlack), AllowPrivateIPs: allowPrivateIPs},
			notifier.Webhook{URL: loadString(&notifyWebhook), Secret: loadString(&notifySecret), AllowPrivateIPs: allowPrivateIPs},
		}}
}

// maxCheckpoints follows the branch.max_checkpoints setting.
var maxCheckpoints = newInt64(domain.DefaultMaxCheckpoints)

// Resource limits follow the branch.refquota_gb, branch.max_per_user and
// branch.max_ttl settings.
var refQuotaGB, maxPerUser, maxBranchTTL = newInt64(0), newInt64(0), newInt64(0)

func resourceLimits() dbbranch.Limits {
	return dbbranch.Limits{
		RefQuotaBytes:      uint64(refQuotaGB.Load()) << 30,
		MaxBranchesPerUser: int(maxPerUser.Load()),
		MaxTTL:             time.Duration(maxBranchTTL.Load()),
	}
}

// pruneSnapshots follows the base.prune_snapshots setting.
var pruneSnapshots = newBool(true)

// validOpenPassword accepts the shared password of open branches: letters
// and digits, so it is safe in any engine's user statement.
func validOpenPassword(pw string) bool {
	if pw == "" || len(pw) > 64 {
		return false
	}
	for _, r := range pw {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

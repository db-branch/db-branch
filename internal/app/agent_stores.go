package app

import (
	"io"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/backupdir"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/infrastructure/uploads"
	"github.com/db-branch/db-branch/internal/usecase/notify"
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

// newNotifier tells Slack and a webhook about expiring branches, failed
// operations and nodes going down. Only the leader checks; after a leader
// change a notice may be sent again.
func newNotifier(st notify.State) *notify.Service {
	return &notify.Service{State: st, Clock: time.Now, Since: time.Now(), DownAfter: NodeDownAfter,
		Warn: func() time.Duration { return time.Duration(notifyWarn.Load()) },
		Senders: []notify.Sender{
			notifier.Slack{URL: loadString(&notifySlack)},
			notifier.Webhook{URL: loadString(&notifyWebhook), Secret: loadString(&notifySecret)},
		}}
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

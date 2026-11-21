package runtimemodule

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// ErrExchangeUnsupported is returned when the OS or filesystem does not support
// atomic directory exchange (Linux renameat2 RENAME_EXCHANGE).
var ErrExchangeUnsupported = errors.New("atomic directory exchange is not supported on this platform")

// ModuleStore manages atomic installation, upgrades, locking, listing, and
// removal of runtime modules under Loader.RootDir().
type ModuleStore struct {
	// Loader verifies installed modules and provides RootDir / KeysDir / Owner.
	Loader Loader
	// Flock acquires or releases an advisory file lock (default: syscall.Flock).
	Flock func(fd int, how int) error
	// RenameExchange atomically swaps two directories on the same filesystem
	// (default: Linux renameat2 RENAME_EXCHANGE; ErrExchangeUnsupported elsewhere).
	RenameExchange func(a, b string) error
	// Rename moves a directory or file (default: os.Rename).
	Rename func(oldpath, newpath string) error
	// Chown sets ownership on directories and the lock file (default: os.Chown).
	Chown func(name string, uid, gid int) error
	// Rand supplies randomness for fallback backup directory names (default: crypto/rand.Reader).
	Rand io.Reader
}

// RootDir returns Loader.RootDir().
func (s ModuleStore) RootDir() string {
	return s.Loader.RootDir()
}

func (s ModuleStore) renameFn() func(oldpath, newpath string) error {
	if s.Rename != nil {
		return s.Rename
	}
	return os.Rename
}

func (s ModuleStore) renameExchangeFn() func(a, b string) error {
	if s.RenameExchange != nil {
		return s.RenameExchange
	}
	return renameExchange
}

func (s ModuleStore) flockFn() func(fd int, how int) error {
	if s.Flock != nil {
		return s.Flock
	}
	return syscall.Flock
}

// Lock acquires an exclusive advisory lock on RootDir/.lock using flock(2),
// respecting ctx cancellation. The returned unlock function is idempotent.
func (s ModuleStore) Lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := s.RootDir()
	if err := s.Loader.checkTrustDir(root); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%w: %s is a symbolic link: %w", ErrUntrusted, lockPath, err)
		}
		return nil, err
	}
	if err := applyChown(s.Chown, lockPath); err != nil {
		_ = f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = s.Loader.checkTrustInfo(lockPath, fi, false, false)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	flock := s.flockFn()
	fd := int(f.Fd())
	for {
		err := flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			var once sync.Once
			unlock := func() {
				once.Do(func() {
					_ = flock(fd, syscall.LOCK_UN)
					_ = f.Close()
				})
			}
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Get verifies and returns the installed module for engine.
// Returns domain.ErrNotFound when the engine directory does not exist.
func (s ModuleStore) Get(engine string) (Loaded, error) {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return Loaded{}, err
	}
	dest := filepath.Join(s.RootDir(), engine)
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return Loaded{}, fmt.Errorf("%w: runtime module %q", domain.ErrNotFound, engine)
	}
	return s.Loader.Load(engine)
}

// List verifies and returns all installed modules under RootDir via Loader.LoadAll.
func (s ModuleStore) List() []Result {
	return s.Loader.LoadAll()
}

// ReadSource reads .source.json for an installed engine.
func (s ModuleStore) ReadSource(engine string) (ModuleSource, error) {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return ModuleSource{}, err
	}
	return s.Loader.ReadModuleSource(filepath.Join(s.RootDir(), engine))
}

// Remove deletes an installed runtime module directory under RootDir.
// Built-in engines, dot-prefixed entries (.staging-*, .lock), and symlinks are refused.
func (s ModuleStore) Remove(engine string) error {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return err
	}
	root := s.RootDir()
	if err := s.Loader.checkTrustDir(root); err != nil {
		return err
	}
	dest := filepath.Join(root, engine)
	fi, err := os.Lstat(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: runtime module %q", domain.ErrNotFound, engine)
		}
		return err
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUntrusted, dest)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return syncDir(root)
}

// Commit atomically moves stageDir into RootDir/<engine>.
// When upgrade is false, an existing engine directory is refused with domain.ErrAlreadyInstalled
// without modifying the existing directory or stageDir permissions.
func (s ModuleStore) Commit(stageDir, engine string, upgrade bool) error {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return err
	}
	root := s.RootDir()
	if err := s.Loader.checkTrustDir(root); err != nil {
		return err
	}
	cleanStage := filepath.Clean(stageDir)
	if filepath.Dir(cleanStage) != filepath.Clean(root) || !strings.HasPrefix(filepath.Base(cleanStage), ".staging-") {
		return fmt.Errorf("%w: staging directory %q must be a direct .staging-* child of %q", ErrUntrusted, stageDir, root)
	}
	if err := s.Loader.checkTrustDir(cleanStage); err != nil {
		return err
	}

	dest := filepath.Join(root, engine)
	_, statErr := os.Lstat(dest)
	if !upgrade {
		if statErr == nil {
			return fmt.Errorf("%w: %s", domain.ErrAlreadyInstalled, engine)
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := prepareStageForCommit(cleanStage); err != nil {
			return err
		}
		if err := s.renameFn()(cleanStage, dest); err != nil {
			if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) {
				return fmt.Errorf("%w: %s", domain.ErrAlreadyInstalled, engine)
			}
			return err
		}
		return syncDir(root)
	}
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: runtime module %q", domain.ErrNotFound, engine)
		}
		return statErr
	}
	if err := prepareStageForCommit(cleanStage); err != nil {
		return err
	}
	err := s.renameExchangeFn()(cleanStage, dest)
	if err == nil {
		// cleanStage now holds the old module directory; remove it.
		if rmErr := os.RemoveAll(cleanStage); rmErr != nil {
			return rmErr
		}
		return syncDir(root)
	}
	if !errors.Is(err, ErrExchangeUnsupported) {
		return err
	}
	return s.commitUpgradeFallback(root, cleanStage, dest)
}

func (s ModuleStore) commitUpgradeFallback(root, stageDir, dest string) error {
	suffix, err := randomHex(s.Rand, 12)
	if err != nil {
		return err
	}
	rename := s.renameFn()
	oldPath := filepath.Join(root, ".old-"+suffix)
	if err := rename(dest, oldPath); err != nil {
		return err
	}
	if err := rename(stageDir, dest); err != nil {
		_ = rename(oldPath, dest)
		return err
	}
	if err := os.RemoveAll(oldPath); err != nil {
		return err
	}
	return syncDir(root)
}

func prepareStageForCommit(stageDir string) error {
	if err := os.Chmod(stageDir, 0o755); err != nil {
		return err
	}
	return syncDir(stageDir)
}

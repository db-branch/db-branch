package runtimemodule_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

type storeEnv struct {
	root    string
	keysDir string
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	loader  runtimemodule.Loader
	ext     runtimemodule.ArchiveExtractor
	store   runtimemodule.ModuleStore
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "runtimes")
	keysDir := filepath.Join(base, "trusted-keys")
	for _, d := range []string{root, keysDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(keysDir, "acme-2026.pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644)
	owner := func(fs.FileInfo) (uint32, bool) { return 0, true }
	chown := func(string, int, int) error { return nil }
	loader := runtimemodule.Loader{Root: root, KeysDir: keysDir, Owner: owner}
	return &storeEnv{
		root:    root,
		keysDir: keysDir,
		pub:     pub,
		priv:    priv,
		loader:  loader,
		ext:     runtimemodule.ArchiveExtractor{Root: root, Owner: owner, Chown: chown},
		store:   runtimemodule.ModuleStore{Loader: loader, Chown: chown},
	}
}

func (e *storeEnv) stageVersion(t *testing.T, engine, version string) *runtimemodule.ExtractedModule {
	t.Helper()
	items, _, _, _ := validModuleItems(t, e.root, engine, version, e.pub, e.priv, "acme-2026", false)
	mod, err := e.ext.ExtractToStaging(context.Background(), buildTarGz(t, items), engine)
	if err != nil {
		t.Fatalf("ExtractToStaging(%s %s): %v", engine, version, err)
	}
	return mod
}

func TestModuleStoreCommit_新規導入を原子的に配置しupgradeなしで既存があれば拒否する(t *testing.T) {
	env := newStoreEnv(t)

	mod1 := env.stageVersion(t, "clickhouse", "0.1.0")
	defer mod1.Cleanup()

	if err := env.store.Commit(mod1.StageDir, "clickhouse", false); err != nil {
		t.Fatalf("Commit(v0.1.0): %v", err)
	}
	fi, err := os.Stat(filepath.Join(env.root, "clickhouse"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("installed dir mode = %04o, want 0755", fi.Mode().Perm())
	}
	assertNoStagingDirs(t, env.root)

	got, err := env.loader.Load("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.1.0" {
		t.Fatalf("Load after Commit = %+v, %v", got, err)
	}

	// Second Commit with upgrade=false must fail with ErrAlreadyInstalled and keep v0.1.0 intact.
	mod2 := env.stageVersion(t, "clickhouse", "0.2.0")
	defer mod2.Cleanup()
	if err := env.store.Commit(mod2.StageDir, "clickhouse", false); !errors.Is(err, domain.ErrAlreadyInstalled) {
		t.Errorf("duplicate Commit(upgrade=false) = %v, want ErrAlreadyInstalled", err)
	}
	stageInfo, err := os.Stat(mod2.StageDir)
	if err != nil {
		t.Fatal(err)
	}
	if stageInfo.Mode().Perm() != 0o700 {
		t.Errorf("rejected stageDir mode = %04o, want 0700", stageInfo.Mode().Perm())
	}
	still, err := env.loader.Load("clickhouse")
	if err != nil || still.Manifest.ModuleVersion != "0.1.0" {
		t.Errorf("existing module changed: %+v, %v", still, err)
	}

	// Invalid engine name or stageDir outside RootDir is refused.
	if err := env.store.Commit(mod2.StageDir, "mysql", false); !errors.Is(err, domain.ErrInvalidModule) {
		t.Errorf("Commit(mysql) = %v, want ErrInvalidModule", err)
	}
	if err := env.store.Commit(t.TempDir(), "clickhouse", false); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Commit(outside root) = %v, want ErrUntrusted", err)
	}
}

func TestModuleStoreCommit_upgrade時にRENAME_EXCHANGEで原子的に差し替え旧ディレクトリを残さない(t *testing.T) {
	env := newStoreEnv(t)

	// Upgrade when nothing is installed yet fails with ErrNotFound.
	mod0 := env.stageVersion(t, "clickhouse", "0.1.0")
	defer mod0.Cleanup()
	if err := env.store.Commit(mod0.StageDir, "clickhouse", true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("upgrade missing engine = %v, want ErrNotFound", err)
	}

	// Fresh install v0.1.0.
	if err := env.store.Commit(mod0.StageDir, "clickhouse", false); err != nil {
		t.Fatalf("fresh install v0.1.0: %v", err)
	}

	// 1. Atomic RENAME_EXCHANGE path (injected swap).
	exchangeCalls := 0
	storeAtomic := env.store
	storeAtomic.RenameExchange = func(a, b string) error {
		exchangeCalls++
		tmp := filepath.Join(env.root, ".swap-sim")
		if err := os.Rename(b, tmp); err != nil {
			return err
		}
		if err := os.Rename(a, b); err != nil {
			_ = os.Rename(tmp, b)
			return err
		}
		return os.Rename(tmp, a)
	}

	mod1 := env.stageVersion(t, "clickhouse", "0.2.0")
	defer mod1.Cleanup()
	if err := storeAtomic.Commit(mod1.StageDir, "clickhouse", true); err != nil {
		t.Fatalf("Commit upgrade v0.2.0 via RenameExchange: %v", err)
	}
	if exchangeCalls != 1 {
		t.Errorf("RenameExchange calls = %d, want 1", exchangeCalls)
	}
	got, err := env.loader.Load("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.2.0" {
		t.Fatalf("Load after atomic upgrade = %+v, %v", got, err)
	}
	assertNoStagingDirs(t, env.root)

	// 2. Fallback path when RENAME_EXCHANGE returns ErrExchangeUnsupported.
	storeFallback := env.store
	storeFallback.RenameExchange = func(a, b string) error {
		return runtimemodule.ErrExchangeUnsupported
	}
	mod2 := env.stageVersion(t, "clickhouse", "0.3.0")
	defer mod2.Cleanup()
	if err := storeFallback.Commit(mod2.StageDir, "clickhouse", true); err != nil {
		t.Fatalf("Commit upgrade v0.3.0 via fallback: %v", err)
	}
	got, err = env.loader.Load("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.3.0" {
		t.Fatalf("Load after fallback upgrade = %+v, %v", got, err)
	}
	assertNoStagingDirs(t, env.root)

	// 3. Default platform RenameExchange (on !linux exercises exchange_other.go -> fallback).
	mod3 := env.stageVersion(t, "clickhouse", "0.4.0")
	defer mod3.Cleanup()
	if err := env.store.Commit(mod3.StageDir, "clickhouse", true); err != nil {
		t.Fatalf("Commit upgrade v0.4.0 via default RenameExchange: %v", err)
	}
	got, err = env.loader.Load("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.4.0" {
		t.Fatalf("Load after default upgrade = %+v, %v", got, err)
	}
	assertNoStagingDirs(t, env.root)

	// 4. Unexpected RenameExchange error propagates without corrupting installed v0.4.0.
	storeFail := env.store
	storeFail.RenameExchange = func(a, b string) error { return errors.New("io error") }
	mod4 := env.stageVersion(t, "clickhouse", "0.5.0")
	defer mod4.Cleanup()
	if err := storeFail.Commit(mod4.StageDir, "clickhouse", true); err == nil {
		t.Errorf("Commit with failing RenameExchange succeeded")
	}
	got, err = env.loader.Load("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.4.0" {
		t.Errorf("existing v0.4.0 corrupted after failed exchange: %+v, %v", got, err)
	}
}

func TestModuleStoreLock_並行実行をflockで排他制御する(t *testing.T) {
	env := newStoreEnv(t)

	unlock1, err := env.store.Lock(context.Background())
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	acquired := make(chan func(), 1)
	errCh := make(chan error, 1)
	go func() {
		u, err := env.store.Lock(context.Background())
		if err != nil {
			errCh <- err
			return
		}
		acquired <- u
	}()

	// Second caller must stay blocked while first holds the lock.
	select {
	case <-acquired:
		t.Fatal("second Lock succeeded while first lock was held")
	case err := <-errCh:
		t.Fatalf("second Lock failed unexpectedly: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	// A caller with a short timeout gets DeadlineExceeded while locked.
	shortCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := env.store.Lock(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timed-out Lock = %v, want context.DeadlineExceeded", err)
	}

	// Releasing first lock unblocks second caller (double unlock is safe).
	unlock1()
	unlock1()

	select {
	case unlock2 := <-acquired:
		unlock2()
	case err := <-errCh:
		t.Fatalf("second Lock after unlock failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after first unlock")
	}

	// Unexpected flock error propagates immediately.
	badFlock := env.store
	badFlock.Flock = func(fd, how int) error { return errors.New("flock failed") }
	if _, err := badFlock.Lock(context.Background()); err == nil {
		t.Errorf("Lock with failing Flock succeeded")
	}

	// Symlink .lock file is refused with ErrUntrusted.
	lockPath := filepath.Join(env.root, ".lock")
	_ = os.Remove(lockPath)
	realLock := filepath.Join(t.TempDir(), "real.lock")
	write(t, realLock, nil, 0o600)
	if err := os.Symlink(realLock, lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Lock(context.Background()); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Lock on symlink .lock = %v, want ErrUntrusted", err)
	}
}

func TestModuleStore_GetとListとRemoveで導入済みモジュールを操作でき不正な名前を拒否する(t *testing.T) {
	env := newStoreEnv(t)

	if _, err := env.store.Get("clickhouse"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if _, err := env.store.Get("mysql"); !errors.Is(err, domain.ErrInvalidModule) {
		t.Errorf("Get(mysql) = %v, want ErrInvalidModule", err)
	}

	mod := env.stageVersion(t, "clickhouse", "0.1.0")
	defer mod.Cleanup()
	wantSrc := runtimemodule.ModuleSource{Repo: "github.com/acme/rt-ch", Tag: "v0.1.0", KeyID: "acme-2026"}
	if err := mod.WriteSource(wantSrc); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Commit(mod.StageDir, "clickhouse", false); err != nil {
		t.Fatal(err)
	}

	got, err := env.store.Get("clickhouse")
	if err != nil || got.Manifest.ModuleVersion != "0.1.0" {
		t.Fatalf("Get(clickhouse) = %+v, %v", got, err)
	}
	src, err := env.store.ReadSource("clickhouse")
	if err != nil || src != wantSrc {
		t.Errorf("ReadSource(clickhouse) = %+v, %v, want %+v", src, err, wantSrc)
	}
	if _, err := env.store.ReadSource("mysql"); !errors.Is(err, domain.ErrInvalidModule) {
		t.Errorf("ReadSource(mysql) = %v, want ErrInvalidModule", err)
	}

	// List returns only real engines, skipping .lock / .staging-*.
	write(t, filepath.Join(env.root, ".staging-tmp", "manifest.json"), []byte("{"), 0o644)
	list := env.store.List()
	if len(list) != 1 || list[0].Engine != "clickhouse" || list[0].Err != nil {
		t.Fatalf("List() = %+v, want [clickhouse]", list)
	}

	// Remove refuses invalid names, dot names, built-in engines, missing engines, and symlinks.
	for _, bad := range []string{"", ".staging-tmp", ".lock", "../evil", "mysql", "postgres"} {
		if err := env.store.Remove(bad); !errors.Is(err, domain.ErrInvalidModule) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidModule", bad, err)
		}
	}
	if err := env.store.Remove("mariadb"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Remove(missing) = %v, want ErrNotFound", err)
	}
	if err := os.Symlink(filepath.Join(env.root, "clickhouse"), filepath.Join(env.root, "duckdb")); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Remove("duckdb"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Remove(symlink) = %v, want ErrUntrusted", err)
	}

	// Valid Remove deletes the module directory.
	if err := env.store.Remove("clickhouse"); err != nil {
		t.Fatalf("Remove(clickhouse): %v", err)
	}
	if _, err := env.store.Get("clickhouse"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Remove = %v, want ErrNotFound", err)
	}
}

func TestModuleStore_フォールバック失敗時のロールバックと各種エラー経路を処理する(t *testing.T) {
	env := newStoreEnv(t)

	// Install v0.1.0 first.
	mod1 := env.stageVersion(t, "clickhouse", "0.1.0")
	defer mod1.Cleanup()
	if err := env.store.Commit(mod1.StageDir, "clickhouse", false); err != nil {
		t.Fatal(err)
	}

	// Fallback upgrade: second Rename(stageDir -> dest) fails => must roll back dest to v0.1.0!
	mod2 := env.stageVersion(t, "clickhouse", "0.2.0")
	defer mod2.Cleanup()
	stRollback := env.store
	stRollback.RenameExchange = func(a, b string) error { return runtimemodule.ErrExchangeUnsupported }
	calls := 0
	stRollback.Rename = func(oldpath, newpath string) error {
		calls++
		if calls == 2 {
			return errors.New("injected second rename failure")
		}
		return os.Rename(oldpath, newpath)
	}
	if err := stRollback.Commit(mod2.StageDir, "clickhouse", true); err == nil {
		t.Fatal("expected rollback error")
	}
	still, err := env.loader.Load("clickhouse")
	if err != nil || still.Manifest.ModuleVersion != "0.1.0" {
		t.Fatalf("rollback failed to restore v0.1.0: %+v, %v", still, err)
	}

	// Fallback upgrade with failing Rand or failing first Rename.
	stBadRand := env.store
	stBadRand.RenameExchange = func(a, b string) error { return runtimemodule.ErrExchangeUnsupported }
	stBadRand.Rand = failReader{err: errors.New("no rand")}
	if err := stBadRand.Commit(mod2.StageDir, "clickhouse", true); err == nil {
		t.Errorf("fallback with failing Rand succeeded")
	}
	stBadFirstRename := env.store
	stBadFirstRename.RenameExchange = func(a, b string) error { return runtimemodule.ErrExchangeUnsupported }
	stBadFirstRename.Rename = func(a, b string) error { return errors.New("rename failed") }
	if err := stBadFirstRename.Commit(mod2.StageDir, "clickhouse", true); err == nil {
		t.Errorf("fallback with failing first Rename succeeded")
	}

	// Fresh Commit race where Rename returns os.ErrExist maps to ErrAlreadyInstalled.
	mod3 := env.stageVersion(t, "mariadb", "0.1.0")
	defer mod3.Cleanup()
	stRace := env.store
	stRace.Rename = func(a, b string) error { return os.ErrExist }
	if err := stRace.Commit(mod3.StageDir, "mariadb", false); !errors.Is(err, domain.ErrAlreadyInstalled) {
		t.Errorf("Commit rename ErrExist = %v, want ErrAlreadyInstalled", err)
	}
	stOtherRenameErr := env.store
	stOtherRenameErr.Rename = func(a, b string) error { return errors.New("io error") }
	if err := stOtherRenameErr.Commit(mod3.StageDir, "mariadb", false); err == nil {
		t.Errorf("Commit rename io error succeeded")
	}

	// Untrusted stageDir (0777).
	if err := os.Chmod(mod3.StageDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Commit(mod3.StageDir, "mariadb", false); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Commit 0777 stage = %v, want ErrUntrusted", err)
	}

	// Lock error paths: cancelled ctx, failing Chown, untrusted .lock mode (0666), untrusted RootDir.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.store.Lock(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Lock cancelled = %v, want context.Canceled", err)
	}
	stBadChown := env.store
	stBadChown.Chown = func(string, int, int) error { return errors.New("chown denied") }
	if _, err := stBadChown.Lock(context.Background()); err == nil {
		t.Errorf("Lock failing Chown succeeded")
	}
	lockPath := filepath.Join(env.root, ".lock")
	write(t, lockPath, nil, 0o666)
	if _, err := env.store.Lock(context.Background()); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Lock 0666 .lock = %v, want ErrUntrusted", err)
	}

	// Untrusted RootDir (0777) rejected by Lock, Commit, Remove.
	if err := os.Chmod(env.root, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Lock(context.Background()); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Lock 0777 root = %v, want ErrUntrusted", err)
	}
	if err := env.store.Commit(mod3.StageDir, "mariadb", false); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Commit 0777 root = %v, want ErrUntrusted", err)
	}
	if err := env.store.Remove("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Remove 0777 root = %v, want ErrUntrusted", err)
	}
}

func TestModuleStoreCommit_入れ替え後の旧ディレクトリ削除に失敗してもコミット成功として扱う(t *testing.T) {
	cases := map[string]func(*runtimemodule.ModuleStore){
		"RENAME_EXCHANGE": func(st *runtimemodule.ModuleStore) {
			st.RenameExchange = func(a, b string) error {
				tmp := a + ".swap"
				if err := os.Rename(b, tmp); err != nil {
					return err
				}
				if err := os.Rename(a, b); err != nil {
					return err
				}
				return os.Rename(tmp, a)
			}
		},
		"fallback": func(st *runtimemodule.ModuleStore) {
			st.RenameExchange = func(a, b string) error { return runtimemodule.ErrExchangeUnsupported }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			env := newStoreEnv(t)
			mod0 := env.stageVersion(t, "clickhouse", "0.1.0")
			defer mod0.Cleanup()
			if err := env.store.Commit(mod0.StageDir, "clickhouse", false); err != nil {
				t.Fatal(err)
			}
			st := env.store
			setup(&st)
			st.RemoveAll = func(string) error { return errors.New("device busy") }
			mod1 := env.stageVersion(t, "clickhouse", "0.2.0")
			defer mod1.Cleanup()
			if err := st.Commit(mod1.StageDir, "clickhouse", true); err != nil {
				t.Fatalf("入れ替えは成功済みなので Commit はエラーを返さない: %v", err)
			}
			got, err := env.loader.Load("clickhouse")
			if err != nil || got.Manifest.ModuleVersion != "0.2.0" {
				t.Fatalf("Load = %+v, %v", got, err)
			}
		})
	}
}

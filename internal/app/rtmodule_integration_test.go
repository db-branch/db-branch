//go:build integration

package app

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

// Runs the runtime module install/upgrade/remove lifecycle end-to-end on real
// Linux root filesystem and useradd/groupadd, using test/fakeruntime under a
// disposable root (/srv/mi-it). Only external I/O boundaries (GitHub HTTP,
// interactive prompt, systemd restart, active usage check) are faked.
func TestRuntimeModules_実ファイルシステムとuseraddで導入から削除まで通す(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if goruntime.GOOS != "linux" {
		t.Skip("needs linux")
	}

	const (
		baseDir   = "/srv/mi-it"
		engine    = "fakedb"
		runAsUser = "db-branch-rt-fakedb"
		keyID     = "fakedb-it-2026"
		repoOwner = "example"
		repoName  = "db-branch-rt-fakedb"
	)
	rootDir := filepath.Join(baseDir, "runtimes")
	keysDir := filepath.Join(baseDir, "trusted-keys")

	_, preUserErr := user.Lookup(runAsUser)
	userExistedBefore := preUserErr == nil
	_, preGroupErr := user.LookupGroup(runAsUser)
	groupExistedBefore := preGroupErr == nil

	if err := os.RemoveAll(baseDir); err != nil {
		t.Fatalf("clean stale %s: %v", baseDir, err)
	}
	for _, dir := range []string{baseDir, rootDir, keysDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.Chown(dir, 0, 0); err != nil {
			t.Fatalf("chown %s: %v", dir, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(baseDir)
		if !userExistedBefore {
			_ = exec.Command("userdel", runAsUser).Run()
		}
		if !groupExistedBefore {
			_ = exec.Command("groupdel", runAsUser).Run()
		}
	})

	binBytes := fakeruntimeBinary(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	fp, err := domain.NewFingerprint(pub)
	if err != nil {
		t.Fatalf("compute fingerprint: %v", err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate alternate key: %v", err)
	}

	arch := goruntime.GOARCH
	assetName := func(version string) string {
		return fmt.Sprintf("db-branch-rt-%s_%s_linux_%s.tar.gz", engine, version, arch)
	}
	makeRef := func(tag string) domain.GitHubRef {
		return domain.GitHubRef{Owner: repoOwner, Repo: repoName, Tag: tag}
	}

	releases := newFakeReleaseSource()
	prompter := &fakePrompter{allow: false}
	restarter := &fakeRestarter{}
	usage := &fakeUsageChecker{}

	svc := newRuntimeModuleService(runtimeModuleWiring{
		rootDir:   rootDir,
		keysDir:   keysDir,
		releases:  releases,
		usage:     usage,
		prompter:  prompter,
		restarter: restarter,
	})
	loader := runtimemodule.Loader{Root: rootDir, KeysDir: keysDir}
	keyStore := runtimemodule.KeyStore{Dir: keysDir}
	ctx := context.Background()

	catV1 := []byte(`[{"version":"1.0"}]`)
	catV2 := []byte(`[{"version":"1.0"},{"version":"1.1"}]`)

	ref010 := makeRef("v0.1.0")
	archive010 := buildFakedbTarGz(t, rootDir, "0.1.0", binBytes, catV1, keyID, pub, priv, true)
	releases.set(ref010, assetName("0.1.0"), archive010)

	ref020 := makeRef("v0.2.0")
	archive020 := buildFakedbTarGz(t, rootDir, "0.2.0", binBytes, catV2, keyID, pub, priv, true)
	releases.set(ref020, assetName("0.2.0"), archive020)

	// 1. 未知の鍵 + 非対話(TrustKey なし、Prompter は拒否) → ErrKeyUntrusted、何も残らない。
	t.Run("1_未知の鍵で拒否されたら何も残らない", func(t *testing.T) {
		_, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref010})
		if !errors.Is(err, domain.ErrKeyUntrusted) {
			t.Fatalf("Install err = %v, want ErrKeyUntrusted", err)
		}
		if prompter.callCount() != 1 {
			t.Errorf("prompter calls = %d, want 1", prompter.callCount())
		}
		if restarter.calls.Load() != 0 {
			t.Errorf("restart calls = %d, want 0", restarter.calls.Load())
		}
		if _, err := os.Lstat(filepath.Join(rootDir, engine)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("module dir unexpectedly exists: %v", err)
		}
		assertNoTempDirs(t, rootDir)
		if _, err := os.Lstat(filepath.Join(keysDir, keyID+".pub")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("trusted key unexpectedly created: %v", err)
		}
		if !userExistedBefore {
			if _, err := user.Lookup(runAsUser); err == nil {
				t.Errorf("OS user %q should not be created on untrusted key failure", runAsUser)
			}
		}
	})

	// 2. 正しい --trust-key の指紋 → 成功、ファイル配置・権限・所有者・鍵・OS ユーザー・Loader・Restart を確認。
	t.Run("2_正しいtrust-key指紋で導入できファイル権限とユーザーと鍵とLoadが揃う", func(t *testing.T) {
		res, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref010, TrustKey: fp})
		if err != nil {
			t.Fatalf("Install with TrustKey: %v", err)
		}
		if !res.Installed || res.Engine != engine || res.Version != "0.1.0" || !res.KeyAdded || !res.Restarted {
			t.Fatalf("unexpected InstallResult: %+v", res)
		}
		if !userExistedBefore && !res.UserCreated {
			t.Errorf("UserCreated = false, want true")
		}
		if restarter.calls.Load() != 1 {
			t.Errorf("restart calls = %d, want 1", restarter.calls.Load())
		}

		modDir := filepath.Join(rootDir, engine)
		assertRootDir(t, modDir, 0o755)
		assertRootFile(t, filepath.Join(modDir, "manifest.json"), 0o644)
		assertRootFile(t, filepath.Join(modDir, "manifest.json.sig"), 0o644)
		assertRootFile(t, filepath.Join(modDir, runAsUser), 0o755)
		assertRootFile(t, filepath.Join(modDir, "runtimes.json"), 0o644)
		assertRootFile(t, filepath.Join(modDir, runtimemodule.SourceMetadataFile), 0o644)
		assertNoTempDirs(t, rootDir)

		assertRootFile(t, filepath.Join(keysDir, keyID+".pub"), 0o644)
		tk, err := keyStore.Get(keyID)
		if err != nil {
			t.Fatalf("KeyStore.Get(%q): %v", keyID, err)
		}
		if !tk.Fingerprint.Equal(fp) || !bytes.Equal(tk.PublicKey, pub) {
			t.Errorf("stored key mismatch: fp=%s want=%s", tk.Fingerprint, fp)
		}

		assertIsolatedUser(t, runAsUser, !userExistedBefore)

		loaded, err := loader.Load(engine)
		if err != nil {
			t.Fatalf("Loader.Load(%q): %v", engine, err)
		}
		if !loaded.Signed || loaded.KeyID != keyID || loaded.Manifest.ModuleVersion != "0.1.0" {
			t.Errorf("unexpected Loaded: %+v", loaded)
		}
	})

	// 3. 同じものをもう一度 install(--upgrade なし) → ErrAlreadyInstalled、既存は変わらない。
	t.Run("3_upgradeなしで再導入するとErrAlreadyInstalledになり既存は不変", func(t *testing.T) {
		manPath := filepath.Join(rootDir, engine, "manifest.json")
		manBefore, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatalf("read manifest before: %v", err)
		}
		restartsBefore := restarter.calls.Load()

		_, err = svc.Install(ctx, rtmodule.InstallRequest{Ref: ref010})
		if !errors.Is(err, domain.ErrAlreadyInstalled) {
			t.Fatalf("re-install err = %v, want ErrAlreadyInstalled", err)
		}
		if restarter.calls.Load() != restartsBefore {
			t.Errorf("restart calls changed from %d to %d", restartsBefore, restarter.calls.Load())
		}
		manAfter, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatalf("read manifest after: %v", err)
		}
		if !bytes.Equal(manBefore, manAfter) {
			t.Errorf("manifest.json changed on ErrAlreadyInstalled")
		}
		assertNoTempDirs(t, rootDir)
	})

	// 4. バージョンを上げた tar.gz で --upgrade → 成功(実カーネル RENAME_EXCHANGE)、旧残骸なし、Load が新版を返す。
	t.Run("4_新版でupgradeするとRENAME_EXCHANGEで切り替わり残骸が残らない", func(t *testing.T) {
		res, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref020, Upgrade: true})
		if err != nil {
			t.Fatalf("upgrade Install: %v", err)
		}
		if !res.Installed || !res.Upgraded || res.PreviousVersion != "0.1.0" || res.Version != "0.2.0" || res.KeyAdded || !res.Restarted {
			t.Fatalf("unexpected upgrade InstallResult: %+v", res)
		}
		if restarter.calls.Load() != 2 {
			t.Errorf("restart calls = %d, want 2", restarter.calls.Load())
		}
		assertNoTempDirs(t, rootDir)

		loaded, err := loader.Load(engine)
		if err != nil {
			t.Fatalf("Loader.Load after upgrade: %v", err)
		}
		if !loaded.Signed || loaded.Manifest.ModuleVersion != "0.2.0" {
			t.Errorf("loaded after upgrade = %+v, want signed 0.2.0", loaded)
		}
		src, err := loader.ReadModuleSource(filepath.Join(rootDir, engine))
		if err != nil {
			t.Fatalf("ReadModuleSource after upgrade: %v", err)
		}
		if src.Tag != "v0.2.0" || src.KeyID != keyID {
			t.Errorf("source metadata = %+v, want tag v0.2.0 key %s", src, keyID)
		}
	})

	// 5. 古い版で --upgrade → ErrDowngradeRefused。
	t.Run("5_古い版でupgradeするとErrDowngradeRefusedになる", func(t *testing.T) {
		restartsBefore := restarter.calls.Load()
		_, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref010, Upgrade: true})
		if !errors.Is(err, domain.ErrDowngradeRefused) {
			t.Fatalf("downgrade err = %v, want ErrDowngradeRefused", err)
		}
		if restarter.calls.Load() != restartsBefore {
			t.Errorf("restart calls changed on downgrade failure")
		}
		loaded, err := loader.Load(engine)
		if err != nil || loaded.Manifest.ModuleVersion != "0.2.0" {
			t.Errorf("installed module changed after refused downgrade: %+v, err=%v", loaded, err)
		}
		assertNoTempDirs(t, rootDir)
	})

	// 6. 登録済みの key_id なのに署名を壊した tar.gz(manifest を 1 バイト変える) → ErrBadSignature、既存不変、信頼鍵も不変。
	t.Run("6_登録済みkey_idでmanifestを1バイト改ざんするとErrBadSignatureになり鍵も既存も不変", func(t *testing.T) {
		keyPath := filepath.Join(keysDir, keyID+".pub")
		keyBefore, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("read key before: %v", err)
		}
		manPath := filepath.Join(rootDir, engine, "manifest.json")
		manBefore, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatalf("read manifest before: %v", err)
		}
		restartsBefore := restarter.calls.Load()

		ref030 := makeRef("v0.3.0")
		tampered := buildTamperedManifestTarGz(t, rootDir, "0.3.0", binBytes, catV2, keyID, otherPub, priv)
		releases.set(ref030, assetName("0.3.0"), tampered)

		_, err = svc.Install(ctx, rtmodule.InstallRequest{Ref: ref030, Upgrade: true})
		if !errors.Is(err, rtmodule.ErrBadSignature) {
			t.Fatalf("tampered install err = %v, want ErrBadSignature", err)
		}
		if restarter.calls.Load() != restartsBefore {
			t.Errorf("restart calls changed on bad signature")
		}
		keyAfter, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("read key after: %v", err)
		}
		if !bytes.Equal(keyBefore, keyAfter) {
			t.Errorf("trusted key was modified by candidate key in tampered archive")
		}
		manAfter, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatalf("read manifest after: %v", err)
		}
		if !bytes.Equal(manBefore, manAfter) {
			t.Errorf("installed manifest changed on bad signature")
		}
		assertNoTempDirs(t, rootDir)
	})

	// 7. 悪意ある tar.gz(setuid ビット付き、`../escape`、シンボリックリンク) → ErrUnsafeArchive、何も残らない。
	t.Run("7_悪意あるtar_gzはすべてErrUnsafeArchiveで拒否されステージを残さない", func(t *testing.T) {
		baseItems := buildFakedbTarItems(t, rootDir, "0.4.0", binBytes, catV2, keyID, pub, priv, false)

		setuidItems := append([]itTarItem(nil), baseItems...)
		setuidItems[0].hdr.Mode = 0o4755

		traversalItems := append([]itTarItem{{
			hdr:  tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644},
			body: []byte("escaped"),
		}}, baseItems...)

		symlinkItems := []itTarItem{{
			hdr: tar.Header{Name: "runtimes.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		}}
		for _, it := range baseItems {
			if it.hdr.Name != "runtimes.json" {
				symlinkItems = append(symlinkItems, it)
			}
		}

		cases := []struct {
			name  string
			tag   string
			items []itTarItem
		}{
			{name: "setuid", tag: "v0.4.0", items: setuidItems},
			{name: "traversal", tag: "v0.4.1", items: traversalItems},
			{name: "symlink", tag: "v0.4.2", items: symlinkItems},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ref := makeRef(tc.tag)
				ver := strings.TrimPrefix(tc.tag, "v")
				releases.set(ref, assetName(ver), packTarGz(t, tc.items))

				_, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref, Upgrade: true})
				if !errors.Is(err, domain.ErrUnsafeArchive) {
					t.Fatalf("%s: err = %v, want ErrUnsafeArchive", tc.name, err)
				}
				assertNoTempDirs(t, rootDir)
			})
		}
		for _, escaped := range []string{filepath.Join(baseDir, "escape"), filepath.Join(rootDir, "escape")} {
			if _, err := os.Lstat(escaped); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("path traversal file exists at %s: %v", escaped, err)
			}
		}
		loaded, err := loader.Load(engine)
		if err != nil || loaded.Manifest.ModuleVersion != "0.2.0" {
			t.Errorf("installed module affected by unsafe archive: %+v, err=%v", loaded, err)
		}
	})

	// 8. UsageChecker が使用中なら ErrInUse で削除されない。使用中でなければ Remove 成功、OS ユーザーと信頼鍵は残る。
	t.Run("8_使用中はErrInUseで削除を拒否し未使用なら削除してもユーザーと鍵は残る", func(t *testing.T) {
		usage.set([]string{"prod"}, nil)
		restartsBefore := restarter.calls.Load()

		_, err := svc.Remove(ctx, engine, false)
		if !errors.Is(err, domain.ErrInUse) {
			t.Fatalf("Remove while in use err = %v, want ErrInUse", err)
		}
		if restarter.calls.Load() != restartsBefore {
			t.Errorf("restart calls changed on refused Remove")
		}
		if _, err := loader.Load(engine); err != nil {
			t.Fatalf("module should remain installed when Remove refused: %v", err)
		}

		usage.set(nil, nil)
		rmRes, err := svc.Remove(ctx, engine, false)
		if err != nil {
			t.Fatalf("Remove when unused: %v", err)
		}
		if !rmRes.Removed || !rmRes.Restarted || rmRes.UserKept != runAsUser || rmRes.KeyIDKept != keyID {
			t.Errorf("unexpected RemoveResult: %+v", rmRes)
		}
		if restarter.calls.Load() != restartsBefore+1 {
			t.Errorf("restart calls = %d, want %d", restarter.calls.Load(), restartsBefore+1)
		}
		if _, err := os.Lstat(filepath.Join(rootDir, engine)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("module dir still exists after Remove: %v", err)
		}
		if _, err := user.Lookup(runAsUser); err != nil {
			t.Errorf("OS user %q should remain after Remove: %v", runAsUser, err)
		}
		if _, err := keyStore.Get(keyID); err != nil {
			t.Errorf("trusted key %q should remain after Remove: %v", keyID, err)
		}
	})

	// 9. LoadAll が .staging-* や .lock のようなドット始まりを読み飛ばす(.staging-x に壊れた manifest を置いても成功)。
	t.Run("9_LoadAllはドット始まりのstagingやlockを読み飛ばす", func(t *testing.T) {
		if _, err := os.Lstat(filepath.Join(rootDir, ".lock")); err != nil {
			t.Fatalf("expected .lock to exist in %s: %v", rootDir, err)
		}
		brokenStaging := filepath.Join(rootDir, ".staging-x")
		if err := os.Mkdir(brokenStaging, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", brokenStaging, err)
		}
		defer func() { _ = os.RemoveAll(brokenStaging) }()
		if err := os.WriteFile(filepath.Join(brokenStaging, "manifest.json"), []byte("{corrupt manifest"), 0o644); err != nil {
			t.Fatalf("write corrupt manifest: %v", err)
		}

		results := loader.LoadAll()
		if len(results) != 0 {
			t.Fatalf("LoadAll returned %d results (%+v), want 0 (dot-prefixed entries skipped)", len(results), results)
		}
	})

	// 10. 並行: 同じ engine の install を 2 つ同時に走らせ、片方だけ成功し、もう片方は ErrAlreadyInstalled になる。
	t.Run("10_同じengineへの並行installは片方だけ成功し壊れた状態を残さない", func(t *testing.T) {
		type outcome struct {
			res rtmodule.InstallResult
			err error
		}
		outcomes := make([]outcome, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range outcomes {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				res, err := svc.Install(ctx, rtmodule.InstallRequest{Ref: ref010})
				outcomes[idx] = outcome{res: res, err: err}
			}(i)
		}
		close(start)
		wg.Wait()

		var succeeded, alreadyInstalled int
		for i, o := range outcomes {
			switch {
			case o.err == nil && o.res.Installed:
				succeeded++
			case errors.Is(o.err, domain.ErrAlreadyInstalled):
				alreadyInstalled++
			default:
				t.Errorf("worker %d unexpected outcome: res=%+v err=%v", i, o.res, o.err)
			}
		}
		if succeeded != 1 || alreadyInstalled != 1 {
			t.Fatalf("concurrent outcomes: succeeded=%d alreadyInstalled=%d (want 1 and 1)", succeeded, alreadyInstalled)
		}

		assertNoTempDirs(t, rootDir)
		loaded, err := loader.Load(engine)
		if err != nil {
			t.Fatalf("Loader.Load after concurrent install: %v", err)
		}
		if !loaded.Signed || loaded.Manifest.ModuleVersion != "0.1.0" {
			t.Errorf("unexpected Loaded after concurrent install: %+v", loaded)
		}
		all := loader.LoadAll()
		if len(all) != 1 || all[0].Engine != engine || all[0].Err != nil || !all[0].Loaded.Signed {
			t.Errorf("LoadAll after concurrent install = %+v", all)
		}
	})
}

type itTarItem struct {
	hdr  tar.Header
	body []byte
}

func fakeruntimeBinary(t *testing.T) []byte {
	t.Helper()
	if src := os.Getenv("DBB_FAKERUNTIME"); src != "" {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read DBB_FAKERUNTIME %q: %v", src, err)
		}
		if len(data) == 0 {
			t.Fatalf("DBB_FAKERUNTIME %q is empty", src)
		}
		return data
	}
	outPath := filepath.Join(t.TempDir(), "db-branch-rt-fakedb")
	cmd := exec.Command("go", "build", "-o", outPath, "github.com/db-branch/db-branch/test/fakeruntime")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build test/fakeruntime: %v\n%s", err, out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read built fakeruntime: %v", err)
	}
	return data
}

func sha256HexBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func buildFakedbTarItems(
	t *testing.T,
	rootDir, version string,
	binBytes, catalogBytes []byte,
	keyID string,
	pub ed25519.PublicKey,
	priv ed25519.PrivateKey,
	includePub bool,
) []itTarItem {
	t.Helper()
	const engine, runAsUser = "fakedb", "db-branch-rt-fakedb"
	m := domain.RuntimeManifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Engine:        engine,
		DisplayName:   "FakeDB",
		ModuleVersion: version,
		APIVersion:    domain.SupportedRuntimeAPIVersion,
		Binary:        filepath.Join(rootDir, engine, runAsUser),
		BinarySHA256:  sha256HexBytes(binBytes),
		Catalog:       filepath.Join(rootDir, engine, "runtimes.json"),
		CatalogSHA256: sha256HexBytes(catalogBytes),
		RunAsUser:     runAsUser,
		Protocols:     []domain.ManifestPort{{Name: "sql", Primary: true}},
		Capabilities:  map[string]bool{"query": true},
		Systemd: domain.ManifestSystemd{
			ServiceType:    "simple",
			KillSignal:     "SIGTERM",
			TimeoutStopSec: 30,
		},
	}
	manBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sig := ed25519.Sign(priv, manBytes)
	sigBytes, err := json.Marshal(map[string]string{
		"key_id":    keyID,
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		t.Fatalf("marshal signature: %v", err)
	}
	items := []itTarItem{
		{hdr: tar.Header{Name: runAsUser, Mode: 0o600, Uid: 1000, Gid: 1000}, body: binBytes},
		{hdr: tar.Header{Name: "manifest.json", Mode: 0o666, Uid: 1000, Gid: 1000}, body: manBytes},
		{hdr: tar.Header{Name: "manifest.json.sig", Mode: 0o666, Uid: 1000, Gid: 1000}, body: sigBytes},
		{hdr: tar.Header{Name: "runtimes.json", Mode: 0o666, Uid: 1000, Gid: 1000}, body: catalogBytes},
	}
	if includePub {
		pubBytes := []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
		items = append(items, itTarItem{
			hdr:  tar.Header{Name: keyID + ".pub", Mode: 0o644},
			body: pubBytes,
		})
	}
	return items
}

func buildFakedbTarGz(
	t *testing.T,
	rootDir, version string,
	binBytes, catalogBytes []byte,
	keyID string,
	pub ed25519.PublicKey,
	priv ed25519.PrivateKey,
	includePub bool,
) []byte {
	t.Helper()
	return packTarGz(t, buildFakedbTarItems(t, rootDir, version, binBytes, catalogBytes, keyID, pub, priv, includePub))
}

// buildTamperedManifestTarGz signs a valid manifest with priv, flips 1 byte in
// manifest.json while keeping it valid JSON/manifest, and bundles candidatePub
// under keyID+".pub" to verify that a registered key is never overwritten.
func buildTamperedManifestTarGz(
	t *testing.T,
	rootDir, version string,
	binBytes, catalogBytes []byte,
	keyID string,
	candidatePub ed25519.PublicKey,
	priv ed25519.PrivateKey,
) []byte {
	t.Helper()
	items := buildFakedbTarItems(t, rootDir, version, binBytes, catalogBytes, keyID, candidatePub, priv, true)
	mutated := bytes.Replace(items[1].body, []byte(`"FakeDB"`), []byte(`"FakeDb"`), 1)
	if bytes.Equal(mutated, items[1].body) {
		t.Fatal("failed to mutate 1 byte in manifest.json")
	}
	items[1].body = mutated
	return packTarGz(t, items)
}

func packTarGz(t *testing.T, items []itTarItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, it := range items {
		hdr := it.hdr
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size == 0 {
			hdr.Size = int64(len(it.body))
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("tar write header %q: %v", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg && len(it.body) > 0 {
			if _, err := tw.Write(it.body); err != nil {
				t.Fatalf("tar write body %q: %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func assertNoTempDirs(t *testing.T, rootDir string) {
	t.Helper()
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", rootDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") || strings.HasPrefix(e.Name(), ".old-") {
			t.Errorf("leftover temp dir %q in %s", e.Name(), rootDir)
		}
	}
}

func assertRootDir(t *testing.T, path string, wantMode fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !fi.Mode().IsDir() {
		t.Errorf("%s is not a directory (mode %s)", path, fi.Mode())
	}
	if fi.Mode().Perm() != wantMode {
		t.Errorf("%s perm = %04o, want %04o", path, fi.Mode().Perm(), wantMode)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		t.Fatalf("%s: stat_t unavailable", path)
	}
	if st.Uid != 0 || st.Gid != 0 {
		t.Errorf("%s owner = %d:%d, want 0:0", path, st.Uid, st.Gid)
	}
}

func assertRootFile(t *testing.T, path string, wantMode fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("%s is not a regular file (mode %s)", path, fi.Mode())
	}
	if fi.Mode().Perm() != wantMode {
		t.Errorf("%s perm = %04o, want %04o", path, fi.Mode().Perm(), wantMode)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		t.Fatalf("%s: stat_t unavailable", path)
	}
	if st.Uid != 0 || st.Gid != 0 {
		t.Errorf("%s owner = %d:%d, want 0:0", path, st.Uid, st.Gid)
	}
}

func assertIsolatedUser(t *testing.T, username string, newlyCreated bool) {
	t.Helper()
	usr, err := user.Lookup(username)
	if err != nil {
		t.Fatalf("user.Lookup(%q): %v", username, err)
	}
	uid, err := strconv.ParseUint(usr.Uid, 10, 32)
	if err != nil || uid == 0 {
		t.Errorf("user %q uid = %q, want non-zero", username, usr.Uid)
	}
	gid, err := strconv.ParseUint(usr.Gid, 10, 32)
	if err != nil || gid == 0 {
		t.Errorf("user %q gid = %q, want non-zero", username, usr.Gid)
	}
	if usr.HomeDir != "/nonexistent" {
		t.Errorf("user %q home = %q, want /nonexistent", username, usr.HomeDir)
	}
	if _, err := os.Stat(usr.HomeDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("user %q home %q unexpectedly exists: %v", username, usr.HomeDir, err)
	}
	shell := lookupPasswdShell(t, username)
	if newlyCreated {
		if shell != "/usr/sbin/nologin" {
			t.Errorf("user %q shell = %q, want /usr/sbin/nologin", username, shell)
		}
	} else {
		switch shell {
		case "/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/false", "/bin/false":
		default:
			t.Errorf("user %q shell = %q, want nologin/false", username, shell)
		}
	}
}

func lookupPasswdShell(t *testing.T, username string) string {
	t.Helper()
	f, err := os.Open("/etc/passwd")
	if err != nil {
		t.Fatalf("open /etc/passwd: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(fields) == 7 && fields[0] == username {
			return fields[6]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan /etc/passwd: %v", err)
	}
	t.Fatalf("user %q not found in /etc/passwd", username)
	return ""
}

type fakeReleaseEntry struct {
	tag       string
	assetName string
	url       string
}

type fakeReleaseSource struct {
	mu       sync.RWMutex
	releases map[string]fakeReleaseEntry
	assets   map[string][]byte
}

func newFakeReleaseSource() *fakeReleaseSource {
	return &fakeReleaseSource{
		releases: map[string]fakeReleaseEntry{},
		assets:   map[string][]byte{},
	}
}

func (f *fakeReleaseSource) set(ref domain.GitHubRef, assetName string, archive []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	url := "fake://" + ref.String() + "/" + assetName
	f.releases[ref.String()] = fakeReleaseEntry{
		tag:       ref.Tag,
		assetName: assetName,
		url:       url,
	}
	f.assets[url] = append([]byte(nil), archive...)
}

func (f *fakeReleaseSource) GetRelease(ctx context.Context, ref domain.GitHubRef) (rtmodule.Release, error) {
	if err := ctx.Err(); err != nil {
		return rtmodule.Release{}, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.releases[ref.String()]
	if !ok {
		return rtmodule.Release{}, fmt.Errorf("%w: fake release %s", domain.ErrNotFound, ref.String())
	}
	data := f.assets[e.url]
	return rtmodule.Release{
		Tag: e.tag,
		Assets: []rtmodule.ReleaseAsset{{
			Name:        e.assetName,
			DownloadURL: e.url,
			Size:        int64(len(data)),
		}},
	}, nil
}

func (f *fakeReleaseSource) DownloadAsset(ctx context.Context, asset rtmodule.ReleaseAsset, maxBytes int64, dst io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.RLock()
	data, ok := f.assets[asset.DownloadURL]
	f.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("%w: fake asset %s", domain.ErrNotFound, asset.DownloadURL)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return 0, fmt.Errorf("asset %s exceeds max bytes %d", asset.Name, maxBytes)
	}
	return io.Copy(dst, bytes.NewReader(data))
}

type fakePrompter struct {
	mu    sync.Mutex
	allow bool
	calls int
}

func (p *fakePrompter) ConfirmTrust(_ context.Context, _ string, _ domain.Fingerprint, _ domain.GitHubRef, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.allow, nil
}

func (p *fakePrompter) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeRestarter struct {
	calls atomic.Int32
}

func (r *fakeRestarter) Restart(_ context.Context) error {
	r.calls.Add(1)
	return nil
}

type fakeUsageChecker struct {
	mu       sync.Mutex
	bases    []string
	branches []string
}

func (u *fakeUsageChecker) set(bases, branches []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bases = append([]string(nil), bases...)
	u.branches = append([]string(nil), branches...)
}

func (u *fakeUsageChecker) InUse(_ context.Context, _ domain.Engine) ([]string, []string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bases...), append([]string(nil), u.branches...), nil
}

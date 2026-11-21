package runtimemodule_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

type tarItem struct {
	hdr  tar.Header
	body []byte
}

func buildTarGz(t *testing.T, items []tarItem) string {
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
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg && len(it.body) > 0 {
			if _, err := tw.Write(it.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "module.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func validModuleItems(t *testing.T, root, engine, version string, pub ed25519.PublicKey, priv ed25519.PrivateKey, keyID string, includePub bool) ([]tarItem, []byte, []byte, []byte) {
	t.Helper()
	bin := []byte("#!/bin/sh\necho " + version + "\n")
	cat := []byte(`[{"version":"24.1"}]`)
	m := domain.RuntimeManifest{
		SchemaVersion: 1, Engine: engine, DisplayName: "ClickHouse", ModuleVersion: version, APIVersion: 1,
		Binary:        filepath.Join(root, engine, "db-branch-rt-"+engine),
		BinarySHA256:  sum(bin),
		Catalog:       filepath.Join(root, engine, "runtimes.json"),
		CatalogSHA256: sum(cat),
		RunAsUser:     domain.RuntimeUserPrefix + engine,
		Protocols:     []domain.ManifestPort{{Name: "http", Primary: true}},
		Capabilities:  map[string]bool{"query": true},
		Systemd:       domain.ManifestSystemd{ServiceType: "simple", KillSignal: "SIGTERM", TimeoutStopSec: 300},
	}
	manBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, manBytes)
	sigBytes, _ := json.Marshal(map[string]string{
		"key_id":    keyID,
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	// Intentionally put the binary BEFORE manifest.json and with non-root uid/gid & wrong mode.
	items := []tarItem{
		{hdr: tar.Header{Name: "db-branch-rt-" + engine, Mode: 0o600, Uid: 1000, Gid: 1000}, body: bin},
		{hdr: tar.Header{Name: "manifest.json", Mode: 0o666, Uid: 1000, Gid: 1000}, body: manBytes},
		{hdr: tar.Header{Name: "manifest.json.sig", Mode: 0o666, Uid: 1000, Gid: 1000}, body: sigBytes},
		{hdr: tar.Header{Name: "runtimes.json", Mode: 0o666, Uid: 1000, Gid: 1000}, body: cat},
	}
	var pubBytes []byte
	if includePub {
		pubBytes = []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
		items = append(items, tarItem{hdr: tar.Header{Name: keyID + ".pub", Mode: 0o644}, body: pubBytes})
	}
	return items, manBytes, sigBytes, pubBytes
}

func assertNoStagingDirs(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") || strings.HasPrefix(e.Name(), ".old-") {
			t.Errorf("leftover temp dir %q in %s", e.Name(), root)
		}
	}
}

func newTestExtractor(t *testing.T) (runtimemodule.ArchiveExtractor, string, *[]string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runtimes")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var chowned []string
	ext := runtimemodule.ArchiveExtractor{
		Root:  root,
		Owner: func(fs.FileInfo) (uint32, bool) { return 0, true },
		Chown: func(name string, uid, gid int) error {
			if uid != 0 || gid != 0 {
				t.Errorf("Chown(%q, %d, %d) want 0, 0", name, uid, gid)
			}
			chowned = append(chowned, name)
			return nil
		},
	}
	return ext, root, &chowned
}

func TestExtractToStaging_正常なtar_gzを0700のステージングへ安全な権限で展開する(t *testing.T) {
	if got := (runtimemodule.ArchiveExtractor{}).RootDir(); got != domain.ManifestRoot {
		t.Fatalf("default RootDir = %q", got)
	}

	ext, root, chowned := newTestExtractor(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	items, manBytes, sigBytes, pubBytes := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", true)
	archive := buildTarGz(t, items)

	got, err := ext.ExtractToStaging(context.Background(), archive, "clickhouse")
	if err != nil {
		t.Fatalf("ExtractToStaging: %v", err)
	}
	defer got.Cleanup()

	if filepath.Dir(got.StageDir) != root || !strings.HasPrefix(filepath.Base(got.StageDir), ".staging-") {
		t.Errorf("StageDir = %q, want under %q/.staging-*", got.StageDir, root)
	}
	stageInfo, err := os.Stat(got.StageDir)
	if err != nil {
		t.Fatal(err)
	}
	if stageInfo.Mode().Perm() != 0o700 {
		t.Errorf("stage mode = %04o, want 0700", stageInfo.Mode().Perm())
	}

	// Binary is 0755, other files are 0644, candidate .pub is NOT on disk.
	wantModes := map[string]fs.FileMode{
		"db-branch-rt-clickhouse": 0o755,
		"manifest.json":           0o644,
		"manifest.json.sig":       0o644,
		"runtimes.json":           0o644,
	}
	for name, wantMode := range wantModes {
		fi, err := os.Stat(filepath.Join(got.StageDir, name))
		if err != nil {
			t.Fatalf("missing staged file %s: %v", name, err)
		}
		if fi.Mode().Perm() != wantMode {
			t.Errorf("%s mode = %04o, want %04o", name, fi.Mode().Perm(), wantMode)
		}
	}
	if _, err := os.Stat(filepath.Join(got.StageDir, "acme-2026.pub")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("candidate pubkey should not be written to staging dir: %v", err)
	}
	if got.CandidateKeyID != "acme-2026" || !bytes.Equal(got.CandidateKeyRaw, pubBytes) {
		t.Errorf("candidate key = (%q, %q)", got.CandidateKeyID, got.CandidateKeyRaw)
	}
	if !bytes.Equal(got.ManifestRaw, manBytes) || !bytes.Equal(got.SignatureRaw, sigBytes) {
		t.Errorf("manifest/signature bytes mismatch")
	}
	if len(*chowned) < 5 {
		t.Errorf("expected Chown on stageDir + 4 files, got %d calls", len(*chowned))
	}

	// Cleanup removes stageDir and is idempotent.
	got.Cleanup()
	got.Cleanup()
	assertNoStagingDirs(t, root)

	// Unexpected file outside allowlist is refused and leaves no staging dir.
	extraItems := append(append([]tarItem(nil), items...), tarItem{
		hdr: tar.Header{Name: "postinst.sh"}, body: []byte("echo evil"),
	})
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, extraItems), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("disallowed file = %v, want ErrUnsafeArchive", err)
	}
	assertNoStagingDirs(t, root)

	// Two .pub files are refused.
	twoPub := append(append([]tarItem(nil), items...), tarItem{
		hdr: tar.Header{Name: "other-2026.pub"}, body: pubBytes,
	})
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, twoPub), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("multiple pub files = %v, want ErrUnsafeArchive", err)
	}
	assertNoStagingDirs(t, root)
}

func TestExtractToStaging_パストラバーサルとネストディレクトリと重複エントリを拒否する(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	baseItems, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)

	badNames := map[string]string{
		"親ディレクトリ参照": "../escape",
		"途中の..":     "a..b",
		"絶対パス":      "/etc/passwd",
		"ネストディレクトリ": "sub/file",
		"ドットスラッシュ":  "./manifest.json",
		"バックスラッシュ":  `sub\file`,
		"制御文字":      "bad\x01name",
		"DEL制御文字":   "bad\x7fname",
		"ドット始まり":    ".source.json",
		"空名前":       "",
	}
	for label, badName := range badNames {
		items := append([]tarItem{{hdr: tar.Header{Name: badName}, body: []byte("x")}}, baseItems...)
		_, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, items), "clickhouse")
		if !errors.Is(err, domain.ErrUnsafeArchive) {
			t.Errorf("%s (%q): err = %v, want ErrUnsafeArchive", label, badName, err)
		}
		assertNoStagingDirs(t, root)
		if _, statErr := os.Stat(filepath.Join(root, "escape")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s wrote escaped file outside staging", label)
		}
	}

	// Duplicate entry name must be refused (including duplicate .pub).
	dupItems := append(append([]tarItem(nil), baseItems...), tarItem{
		hdr: tar.Header{Name: "runtimes.json"}, body: []byte(`[]`),
	})
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, dupItems), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("duplicate entry = %v, want ErrUnsafeArchive", err)
	}
	assertNoStagingDirs(t, root)
}

func TestExtractToStaging_シンボリックリンクとハードリンクと特殊ファイルを拒否する(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	baseItems, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)

	cases := map[string]tar.Header{
		"symlink": {Name: "runtimes.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"link":    {Name: "runtimes.json", Typeflag: tar.TypeLink, Linkname: "manifest.json"},
		"fifo":    {Name: "runtimes.json", Typeflag: tar.TypeFifo},
		"char":    {Name: "runtimes.json", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
		"block":   {Name: "runtimes.json", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0},
		"dir":     {Name: "subdir", Typeflag: tar.TypeDir, Mode: 0o755},
	}
	for label, hdr := range cases {
		items := []tarItem{{hdr: hdr}}
		for _, bi := range baseItems {
			if bi.hdr.Name != hdr.Name {
				items = append(items, bi)
			}
		}
		_, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, items), "clickhouse")
		if !errors.Is(err, domain.ErrUnsafeArchive) {
			t.Errorf("%s: err = %v, want ErrUnsafeArchive", label, err)
		}
		assertNoStagingDirs(t, root)
	}
}

func TestExtractToStaging_setuidとsetgidとstickyビット付きエントリを拒否する(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	baseItems, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)

	for label, mode := range map[string]int64{
		"setuid": 0o4755,
		"setgid": 0o2755,
		"sticky": 0o1755,
	} {
		items := append([]tarItem(nil), baseItems...)
		items[0].hdr.Mode = mode
		_, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, items), "clickhouse")
		if !errors.Is(err, domain.ErrUnsafeArchive) {
			t.Errorf("%s (%04o): err = %v, want ErrUnsafeArchive", label, mode, err)
		}
		assertNoStagingDirs(t, root)
	}
}

func TestExtractToStaging_合計サイズ上限と単一ファイル上限とファイル数上限を超えるtar爆弾を拒否する(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	ext.MaxFileBytes = 8 << 10   // 8 KiB
	ext.MaxTotalBytes = 12 << 10 // 12 KiB
	ext.MaxFiles = 16

	// 1. Single-file gzip bomb (2 MiB of zeros in one entry > 8 KiB MaxFileBytes).
	bomb := buildTarGz(t, []tarItem{
		{hdr: tar.Header{Name: "db-branch-rt-clickhouse"}, body: make([]byte, 2<<20)},
	})
	if _, err := ext.ExtractToStaging(context.Background(), bomb, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("single-file tar bomb = %v, want ErrUnsafeArchive with limit exceeded", err)
	}
	assertNoStagingDirs(t, root)

	// 2. Total uncompressed size exceeds MaxTotalBytes across two files each <= MaxFileBytes.
	twoBig := buildTarGz(t, []tarItem{
		{hdr: tar.Header{Name: "db-branch-rt-clickhouse"}, body: make([]byte, 7<<10)},
		{hdr: tar.Header{Name: "runtimes.json"}, body: make([]byte, 7<<10)},
	})
	if _, err := ext.ExtractToStaging(context.Background(), twoBig, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("total size exceeded = %v, want ErrUnsafeArchive with limit exceeded", err)
	}
	assertNoStagingDirs(t, root)

	// 3. Trailing gzip zero-block bomb (2 MiB of zero bytes compressed directly as gzip).
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(make([]byte, 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	zeroBombPath := filepath.Join(t.TempDir(), "zerobomb.tar.gz")
	write(t, zeroBombPath, gzBuf.Bytes(), 0o644)
	if _, err := ext.ExtractToStaging(context.Background(), zeroBombPath, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("gzip zero bomb = %v, want ErrUnsafeArchive with limit exceeded", err)
	}
	assertNoStagingDirs(t, root)

	// 4. More than 16 files exceeds MaxFiles.
	var many []tarItem
	for i := 0; i < 17; i++ {
		many = append(many, tarItem{
			hdr:  tar.Header{Name: fmt.Sprintf("f%02d", i)},
			body: []byte("a"),
		})
	}
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, many), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("17 files = %v, want ErrUnsafeArchive with limit exceeded", err)
	}
	assertNoStagingDirs(t, root)
}

func TestExtractedModule_出所情報source_jsonをステージ内に書き込みLoadに影響せず読み出せる(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	keysDir := filepath.Join(t.TempDir(), "trusted-keys")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(keysDir, "acme-2026.pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644)

	items, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)
	mod, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, items), "clickhouse")
	if err != nil {
		t.Fatalf("ExtractToStaging: %v", err)
	}
	defer mod.Cleanup()

	l := runtimemodule.Loader{
		Root:    root,
		KeysDir: keysDir,
		Owner:   func(fs.FileInfo) (uint32, bool) { return 0, true },
	}
	// Before writing .source.json, ReadModuleSource returns ErrNotFound.
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ReadModuleSource before write = %v, want ErrNotFound", err)
	}

	// Invalid source fields are refused.
	for _, bad := range []runtimemodule.ModuleSource{
		{Repo: "", Tag: "v0.2.0", KeyID: "acme-2026"},
		{Repo: "github.com/acme/rt", Tag: "", KeyID: "acme-2026"},
		{Repo: "github.com/acme/rt", Tag: "v0.2.0", KeyID: "../evil"},
	} {
		if err := mod.WriteSource(bad); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("WriteSource(%+v) = %v, want ErrInvalidRef", bad, err)
		}
	}

	want := runtimemodule.ModuleSource{
		Repo:  "github.com/acme/db-branch-rt-clickhouse",
		Tag:   "v0.2.0",
		KeyID: "acme-2026",
	}
	if err := mod.WriteSource(want); err != nil {
		t.Fatalf("WriteSource: %v", err)
	}

	srcPath := filepath.Join(mod.StageDir, runtimemodule.SourceMetadataFile)
	fi, err := os.Stat(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf(".source.json mode = %04o, want 0644", fi.Mode().Perm())
	}

	// Loader.VerifyDir ignores .source.json and still verifies signature and digests.
	if _, err := l.VerifyDir(mod.StageDir, "clickhouse", true); err != nil {
		t.Errorf("VerifyDir after WriteSource: %v", err)
	}

	got, err := l.ReadModuleSource(mod.StageDir)
	if err != nil {
		t.Fatalf("ReadModuleSource: %v", err)
	}
	if got != want {
		t.Errorf("ReadModuleSource = %+v, want %+v", got, want)
	}

	// Untrusted permissions or malformed JSON are rejected on read.
	if err := os.Chmod(srcPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("ReadModuleSource(0666) = %v, want ErrUntrusted", err)
	}
	write(t, srcPath, []byte(`{"repo":"a","tag":"v1","key_id":"k","extra":1}`), 0o644)
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("ReadModuleSource(unknown field) = %v, want ErrInvalidRef", err)
	}
	write(t, srcPath, []byte(`{"repo":"a","tag":"v1","key_id":"k"}{"x":1}`), 0o644)
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("ReadModuleSource(trailing) = %v, want ErrInvalidRef", err)
	}
	write(t, srcPath, []byte(`{"repo":"","tag":"v1","key_id":"k"}`), 0o644)
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("ReadModuleSource(empty repo) = %v, want ErrInvalidRef", err)
	}

	// Nil / empty ExtractedModule and untrusted dir are refused.
	var nilMod *runtimemodule.ExtractedModule
	if err := nilMod.WriteSource(want); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("nil WriteSource = %v, want ErrInvalidRef", err)
	}
	badMod := &runtimemodule.ExtractedModule{StageDir: filepath.Join(t.TempDir(), "missing")}
	if err := badMod.WriteSource(want); err == nil {
		t.Errorf("WriteSource to missing dir succeeded")
	}
	if os.Geteuid() != 0 {
		noChownMod := &runtimemodule.ExtractedModule{StageDir: mod.StageDir}
		if err := noChownMod.WriteSource(want); err == nil {
			t.Errorf("WriteSource with default os.Chown as non-root unexpectedly succeeded")
		}
	}
	if err := os.Chmod(mod.StageDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadModuleSource(mod.StageDir); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("ReadModuleSource on 0777 dir = %v, want ErrUntrusted", err)
	}
}

func TestExtractToStaging_破損アーカイブや契約違反マニフェストや環境エラーを拒否しステージを残さない(t *testing.T) {
	ext, root, _ := newTestExtractor(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	items, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)
	validArchive := buildTarGz(t, items)

	// Cancelled context before start.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ext.ExtractToStaging(cctx, validArchive, "clickhouse"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx = %v, want context.Canceled", err)
	}

	// Invalid engine name.
	if _, err := ext.ExtractToStaging(context.Background(), validArchive, "mysql"); !errors.Is(err, domain.ErrInvalidModule) {
		t.Errorf("builtin engine = %v, want ErrInvalidModule", err)
	}

	// Missing archive file.
	if _, err := ext.ExtractToStaging(context.Background(), filepath.Join(t.TempDir(), "nope.tar.gz"), "clickhouse"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing archive = %v, want ErrNotExist", err)
	}

	// Invalid gzip file.
	notGzip := filepath.Join(t.TempDir(), "bad.tar.gz")
	write(t, notGzip, []byte("not gzip at all"), 0o644)
	if _, err := ext.ExtractToStaging(context.Background(), notGzip, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("not gzip = %v, want ErrUnsafeArchive", err)
	}

	// Corrupt tar payload inside valid gzip.
	var badTarBuf bytes.Buffer
	gz := gzip.NewWriter(&badTarBuf)
	_, _ = gz.Write([]byte("not a valid tar header block"))
	_ = gz.Close()
	badTarPath := filepath.Join(t.TempDir(), "badtar.tar.gz")
	write(t, badTarPath, badTarBuf.Bytes(), 0o644)
	if _, err := ext.ExtractToStaging(context.Background(), badTarPath, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("corrupt tar = %v, want ErrUnsafeArchive", err)
	}

	// Invalid bundled .pub key_id and oversized .pub (>4 KiB).
	badPubID := append(append([]tarItem(nil), items...), tarItem{hdr: tar.Header{Name: "bad key.pub"}, body: []byte("x")})
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, badPubID), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("invalid pub name = %v, want ErrUnsafeArchive", err)
	}
	bigPub := append(append([]tarItem(nil), items...), tarItem{hdr: tar.Header{Name: "big-key.pub"}, body: make([]byte, 5<<10)})
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, bigPub), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("oversized pub = %v, want ErrUnsafeArchive", err)
	}
	bigSig := append([]tarItem(nil), items...)
	bigSig[2] = tarItem{hdr: tar.Header{Name: "manifest.json.sig"}, body: make([]byte, 70<<10)}
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, bigSig), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("oversized sig = %v, want ErrUnsafeArchive", err)
	}
	bigMan := append([]tarItem(nil), items...)
	bigMan[1] = tarItem{hdr: tar.Header{Name: "manifest.json"}, body: make([]byte, (1<<20)+10)}
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, bigMan), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("oversized manifest = %v, want ErrUnsafeArchive", err)
	}

	// Missing manifest.json.
	noManifest := buildTarGz(t, []tarItem{{hdr: tar.Header{Name: "runtimes.json"}, body: []byte(`[]`)}})
	if _, err := ext.ExtractToStaging(context.Background(), noManifest, "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("missing manifest = %v, want ErrUnsafeArchive", err)
	}

	// Corrupt manifest.json and mismatched engine.
	badManifest := buildTarGz(t, []tarItem{{hdr: tar.Header{Name: "manifest.json"}, body: []byte(`{`)}})
	if _, err := ext.ExtractToStaging(context.Background(), badManifest, "clickhouse"); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("corrupt manifest = %v, want ErrInvalidManifest", err)
	}
	if _, err := ext.ExtractToStaging(context.Background(), validArchive, "mariadb"); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("engine mismatch = %v, want ErrInvalidManifest", err)
	}

	// Manifest binary basename collides with reserved filename.
	collideItems, _, _, _ := validModuleItems(t, root, "clickhouse", "0.2.0", pub, priv, "acme-2026", false)
	var m domain.RuntimeManifest
	_ = json.Unmarshal(collideItems[1].body, &m)
	m.Binary = filepath.Join(root, "clickhouse", "manifest.json")
	collideItems[1].body, _ = json.Marshal(m)
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, collideItems), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("reserved binary name = %v, want ErrUnsafeArchive", err)
	}

	// Missing binary declared in manifest.
	noBin := []tarItem{items[1], items[2], items[3]}
	if _, err := ext.ExtractToStaging(context.Background(), buildTarGz(t, noBin), "clickhouse"); !errors.Is(err, domain.ErrUnsafeArchive) {
		t.Errorf("missing binary = %v, want ErrUnsafeArchive", err)
	}

	// Failing Rand and failing Chown.
	extRand := ext
	extRand.Rand = failReader{err: errors.New("no rand")}
	if _, err := extRand.ExtractToStaging(context.Background(), validArchive, "clickhouse"); err == nil {
		t.Errorf("failing Rand succeeded")
	}
	extChown := ext
	extChown.Chown = func(string, int, int) error { return errors.New("chown denied") }
	if _, err := extChown.ExtractToStaging(context.Background(), validArchive, "clickhouse"); err == nil {
		t.Errorf("failing Chown succeeded")
	}
	// Failing Chown only on file (not stageDir).
	extFileChown := ext
	extFileChown.Chown = func(name string, uid, gid int) error {
		if filepath.Dir(name) != root {
			return errors.New("file chown denied")
		}
		return nil
	}
	if _, err := extFileChown.ExtractToStaging(context.Background(), validArchive, "clickhouse"); err == nil {
		t.Errorf("failing file Chown succeeded")
	}

	// Untrusted root directory.
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ext.ExtractToStaging(context.Background(), validArchive, "clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("0777 root = %v, want ErrUntrusted", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	assertNoStagingDirs(t, root)
}

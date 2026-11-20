package runtimemodule_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

// tree is an installed runtime module (root/clickhouse/...) with a trusted key.
type tree struct {
	t       *testing.T
	root    string
	keys    string
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	binary  string
	catalog string
	man     string
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
}

func newTree(t *testing.T) *tree {
	t.Helper()
	dir := t.TempDir()
	tr := &tree{t: t, root: filepath.Join(dir, "runtimes"), keys: filepath.Join(dir, "trusted-keys")}
	var err error
	if tr.pub, tr.priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tr.keys, "vendor-2026.pub"), []byte(base64.StdEncoding.EncodeToString(tr.pub)+"\n"), 0o644)
	tr.binary = filepath.Join(tr.root, "clickhouse", "db-branch-rt-clickhouse")
	tr.catalog = filepath.Join(tr.root, "clickhouse", "runtimes.json")
	tr.man = filepath.Join(tr.root, "clickhouse", "manifest.json")
	write(t, tr.binary, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	write(t, tr.catalog, []byte(`[]`), 0o644)
	tr.writeManifest(nil)
	tr.sign("vendor-2026", tr.priv)
	return tr
}

// writeManifest writes manifest.json from the files as they are now.
func (tr *tree) writeManifest(mutate func(*domain.RuntimeManifest)) {
	tr.t.Helper()
	bin, _ := os.ReadFile(tr.binary)
	cat, _ := os.ReadFile(tr.catalog)
	m := domain.RuntimeManifest{
		SchemaVersion: 1, Engine: "clickhouse", DisplayName: "ClickHouse", ModuleVersion: "0.1.0", APIVersion: 1,
		Binary: tr.binary, BinarySHA256: sum(bin), Catalog: tr.catalog, CatalogSHA256: sum(cat),
		Protocols:    []domain.ManifestPort{{Name: "http", Primary: true}},
		Capabilities: map[string]bool{"query": true},
		Systemd:      domain.ManifestSystemd{ServiceType: "simple", KillSignal: "SIGTERM", TimeoutStopSec: 300},
	}
	if mutate != nil {
		mutate(&m)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		tr.t.Fatal(err)
	}
	write(tr.t, tr.man, b, 0o644)
}

// sign writes manifest.json.sig for the manifest as it is now.
func (tr *tree) sign(keyID string, priv ed25519.PrivateKey) {
	tr.t.Helper()
	b, _ := os.ReadFile(tr.man)
	sig := ed25519.Sign(priv, b)
	out, _ := json.Marshal(map[string]string{"key_id": keyID, "signature": base64.StdEncoding.EncodeToString(sig)})
	write(tr.t, tr.man+".sig", out, 0o644)
}

func (tr *tree) loader() runtimemodule.Loader {
	return runtimemodule.Loader{
		Root: tr.root, KeysDir: tr.keys,
		Owner: func(fs.FileInfo) (uint32, bool) { return 0, true }, // files owned by root; a test cannot chown
	}
}

func TestLoad_署名の正しいモジュールを読む(t *testing.T) {
	tr := newTree(t)
	got, err := tr.loader().Load("clickhouse")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Signed || got.KeyID != "vendor-2026" || got.Warning != "" {
		t.Errorf("loaded = %+v", got)
	}
	if got.Manifest.Engine != "clickhouse" || got.Manifest.RunAsUser != "db-branch-rt-clickhouse" || got.Manifest.Binary != tr.binary {
		t.Errorf("manifest = %+v", got.Manifest)
	}
}

func TestLoad_署名のないモジュールは既定で拒否し_設定すれば警告つきで読む(t *testing.T) {
	tr := newTree(t)
	if err := os.Remove(tr.man + ".sig"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUnsigned) {
		t.Fatalf("unsigned by default = %v, want ErrUnsigned", err)
	}
	l := tr.loader()
	l.AllowUnsigned = true
	got, err := l.Load("clickhouse")
	if err != nil || got.Signed || !strings.Contains(got.Warning, "unsigned") {
		t.Errorf("allowed unsigned = %+v, %v", got, err)
	}
}

func TestLoad_壊れた署名はAllowUnsignedでも拒否する(t *testing.T) {
	tr := newTree(t)
	tr.writeManifest(func(m *domain.RuntimeManifest) { m.DisplayName = "Tampered" }) // changed after signing
	for _, allow := range []bool{false, true} {
		l := tr.loader()
		l.AllowUnsigned = allow
		if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("AllowUnsigned=%v: tampered manifest = %v, want ErrBadSignature", allow, err)
		}
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader) // signed with a key that is not the trusted one
	tr2 := newTree(t)
	tr2.sign("vendor-2026", other)
	if _, err := tr2.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
		t.Errorf("signed with another key = %v", err)
	}
}

func TestLoad_信頼していない鍵や危険な鍵名の署名を拒否する(t *testing.T) {
	tr := newTree(t)
	for _, id := range []string{"unknown-key", "../vendor-2026", "vendor-2026/../vendor-2026", "", "a b", "vendor-2026\n"} {
		tr.sign(id, tr.priv)
		l := tr.loader()
		l.AllowUnsigned = true
		if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("key_id %q = %v, want ErrBadSignature", id, err)
		}
	}
	// a signature file that is not what it should be
	for _, junk := range []string{"not json", `{"key_id":"vendor-2026"}`, `{"key_id":"vendor-2026","signature":"!!!"}`, `{"key_id":"vendor-2026","signature":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`} {
		write(t, tr.man+".sig", []byte(junk), 0o644)
		if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("signature file %q = %v, want ErrBadSignature", junk, err)
		}
	}
}

func TestLoad_バイナリとカタログがマニフェストのダイジェストと違えば拒否する(t *testing.T) {
	tr := newTree(t)
	write(t, tr.binary, []byte("#!/bin/sh\nrm -rf /\n"), 0o755)
	if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrDigestMismatch) {
		t.Errorf("replaced binary = %v, want ErrDigestMismatch", err)
	}
	tr = newTree(t)
	write(t, tr.catalog, []byte(`[{"url":"https://evil.example"}]`), 0o644)
	l := tr.loader()
	l.AllowUnsigned = true
	if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrDigestMismatch) {
		t.Errorf("replaced catalog = %v, want ErrDigestMismatch", err)
	}
}

func TestLoad_所有者と権限と種類が安全でなければ拒否する(t *testing.T) {
	cases := map[string]func(tr *tree) runtimemodule.Loader{
		"バイナリが他人の所有": func(tr *tree) runtimemodule.Loader {
			l := tr.loader()
			l.Owner = func(fi fs.FileInfo) (uint32, bool) {
				if fi.Name() == "db-branch-rt-clickhouse" {
					return 1000, true
				}
				return 0, true
			}
			return l
		},
		"所有者が分からない": func(tr *tree) runtimemodule.Loader {
			l := tr.loader()
			l.Owner = func(fs.FileInfo) (uint32, bool) { return 0, false }
			return l
		},
		"バイナリがグループ書き込み可": func(tr *tree) runtimemodule.Loader {
			_ = os.Chmod(tr.binary, 0o775)
			return tr.loader()
		},
		"マニフェストが誰でも書ける": func(tr *tree) runtimemodule.Loader {
			_ = os.Chmod(tr.man, 0o666)
			return tr.loader()
		},
		"ディレクトリが誰でも書ける": func(tr *tree) runtimemodule.Loader {
			_ = os.Chmod(filepath.Dir(tr.man), 0o777)
			return tr.loader()
		},
		"バイナリが実行できない": func(tr *tree) runtimemodule.Loader {
			_ = os.Chmod(tr.binary, 0o644)
			return tr.loader()
		},
		"バイナリがシンボリックリンク": func(tr *tree) runtimemodule.Loader {
			real := tr.binary + ".real"
			_ = os.Rename(tr.binary, real)
			_ = os.Symlink(real, tr.binary)
			return tr.loader()
		},
		"マニフェストがシンボリックリンク": func(tr *tree) runtimemodule.Loader {
			real := tr.man + ".real"
			_ = os.Rename(tr.man, real)
			_ = os.Symlink(real, tr.man)
			return tr.loader()
		},
		"鍵ファイルが誰でも書ける": func(tr *tree) runtimemodule.Loader {
			_ = os.Chmod(filepath.Join(tr.keys, "vendor-2026.pub"), 0o666)
			return tr.loader()
		},
	}
	for name, setup := range cases {
		tr := newTree(t)
		l := setup(tr)
		if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
			t.Errorf("%s: err = %v, want ErrUntrusted", name, err)
		}
	}
}

func TestLoad_マニフェストの規則違反はAllowUnsignedでも拒否する(t *testing.T) {
	for name, mutate := range map[string]func(*domain.RuntimeManifest){
		"rootで動かす":      func(m *domain.RuntimeManifest) { m.RunAsUser = "root" },
		"db-branchを名乗る": func(m *domain.RuntimeManifest) { m.RunAsUser = "db-branch" },
		"組み込みのエンジン名":    func(m *domain.RuntimeManifest) { m.Engine = "mysql" },
	} {
		tr := newTree(t)
		tr.writeManifest(mutate)
		tr.sign("vendor-2026", tr.priv)
		l := tr.loader()
		l.AllowUnsigned = true
		if _, err := l.Load("clickhouse"); !errors.Is(err, domain.ErrInvalidManifest) {
			t.Errorf("%s: err = %v, want ErrInvalidManifest", name, err)
		}
	}
	tr := newTree(t) // the manifest names another engine than its directory
	tr.writeManifest(func(m *domain.RuntimeManifest) {
		m.Engine = "other"
		m.Binary = filepath.Join(tr.root, "other", "x")
		m.Catalog = filepath.Join(tr.root, "other", "y")
	})
	tr.sign("vendor-2026", tr.priv)
	if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("engine differs from the directory = %v", err)
	}
}

func TestLoad_エンジン名が不正または存在しなければエラー(t *testing.T) {
	tr := newTree(t)
	for _, e := range []string{"", "..", "../clickhouse", "a/b", "Click House", "missing"} {
		if _, err := tr.loader().Load(e); err == nil {
			t.Errorf("Load(%q) succeeded", e)
		}
	}
}

func TestLoadAll_エンジンごとに結果を返し壊れたものが他を止めない(t *testing.T) {
	tr := newTree(t)
	write(t, filepath.Join(tr.root, "broken", "manifest.json"), []byte("{"), 0o644) // no signature, not a manifest
	write(t, filepath.Join(tr.root, "README"), []byte("x"), 0o644)                  // a file, not an engine
	got := tr.loader().LoadAll()
	if len(got) != 2 || got[0].Engine != "broken" || got[1].Engine != "clickhouse" {
		t.Fatalf("results = %+v", got)
	}
	if got[0].Err == nil || got[1].Err != nil || !got[1].Loaded.Signed {
		t.Errorf("results = %+v", got)
	}
	if empty := (runtimemodule.Loader{Root: filepath.Join(t.TempDir(), "none"), KeysDir: tr.keys}).LoadAll(); len(empty) != 0 {
		t.Errorf("no runtimes directory: %+v", empty)
	}
}

func TestLoader_既定の場所は設計書どおり(t *testing.T) {
	l := runtimemodule.Loader{}
	if l.RootDir() != domain.ManifestRoot || l.TrustedKeysDir() != "/etc/db-branch/trusted-keys" {
		t.Errorf("defaults = %q, %q", l.RootDir(), l.TrustedKeysDir())
	}
}

func TestLoad_setuidとsetgidとstickyのファイルは拒否する(t *testing.T) {
	for name, mode := range map[string]os.FileMode{
		"setuid": os.ModeSetuid | 0o755, "setgid": os.ModeSetgid | 0o755, "sticky": os.ModeSticky | 0o755,
	} {
		tr := newTree(t)
		if err := os.Chmod(tr.binary, mode); err != nil {
			t.Skipf("cannot set %s here: %v", name, err)
		}
		l := tr.loader()
		l.AllowUnsigned = true
		if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
			t.Errorf("%s binary: err = %v, want ErrUntrusted", name, err)
		}
	}
}

func TestLoad_署名はマニフェストが指すファイルを開く前に検証する(t *testing.T) {
	tr := newTree(t)
	tr.writeManifest(func(m *domain.RuntimeManifest) { m.DisplayName = "Tampered" })
	if err := os.Remove(tr.binary); err != nil { // nothing the manifest points at can even be read
		t.Fatal(err)
	}
	if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
		t.Errorf("tampered manifest with a missing binary = %v, want ErrBadSignature first", err)
	}
	tr2 := newTree(t)
	if err := os.Remove(tr2.man + ".sig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tr2.binary); err != nil {
		t.Fatal(err)
	}
	if _, err := tr2.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUnsigned) {
		t.Errorf("unsigned with a missing binary = %v, want ErrUnsigned first", err)
	}
}

func TestLoad_署名ファイルは厳密に読み_未知の項目や後ろのゴミを許さない(t *testing.T) {
	tr := newTree(t)
	good, _ := os.ReadFile(tr.man + ".sig")
	for name, body := range map[string]string{
		"未知の項目": strings.Replace(string(good), `{`, `{"extra":1,`, 1),
		"後ろのゴミ": string(good) + `{"x":1}`,
		"大きすぎる": string(good) + strings.Repeat(" ", 100<<10),
	} {
		write(t, tr.man+".sig", []byte(body), 0o644)
		l := tr.loader()
		l.AllowUnsigned = true
		if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestLoad_鍵ファイルが壊れていたり大きすぎれば拒否する(t *testing.T) {
	tr := newTree(t)
	keyFile := filepath.Join(tr.keys, "vendor-2026.pub")
	for name, body := range map[string]string{
		"base64ではない": "!!!not base64!!!",
		"長さが違う":      base64.StdEncoding.EncodeToString([]byte("short")),
		"大きすぎる":      strings.Repeat("A", 8<<10),
		"空":          "",
	} {
		write(t, keyFile, []byte(body), 0o644)
		if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestLoad_大きすぎるマニフェストは読まずに拒否する(t *testing.T) {
	tr := newTree(t)
	write(t, tr.man, []byte(strings.Repeat(" ", 2<<20)), 0o644)
	if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("a 2 MiB manifest = %v, want ErrUntrusted", err)
	}
}

func TestLoad_親のディレクトリと鍵のディレクトリも安全でなければ拒否する(t *testing.T) {
	cases := map[string]func(tr *tree){
		"runtimesのルートが誰でも書ける": func(tr *tree) { _ = os.Chmod(tr.root, 0o777) },
		"鍵のディレクトリが誰でも書ける":     func(tr *tree) { _ = os.Chmod(tr.keys, 0o777) },
		"エンジンのディレクトリがシンボリックリンク": func(tr *tree) {
			real := filepath.Join(filepath.Dir(tr.root), "real-clickhouse")
			_ = os.Rename(filepath.Join(tr.root, "clickhouse"), real)
			_ = os.Symlink(real, filepath.Join(tr.root, "clickhouse"))
		},
	}
	for name, setup := range cases {
		tr := newTree(t)
		setup(tr)
		if _, err := tr.loader().Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
			t.Errorf("%s: err = %v, want ErrUntrusted", name, err)
		}
	}
}

func TestLoad_所有者を問い合わせる既定の経路はroot以外の所有を拒否する(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("everything created here is owned by root")
	}
	tr := newTree(t)
	l := tr.loader()
	l.Owner = nil // the real file system: these files belong to the user running the test
	if _, err := l.Load("clickhouse"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("files owned by uid %d = %v, want ErrUntrusted", os.Geteuid(), err)
	}
}

func TestLoadAll_シンボリックリンクのエンジンディレクトリは黙って飛ばさず拒否として報告する(t *testing.T) {
	tr := newTree(t)
	if err := os.Symlink(filepath.Join(tr.root, "clickhouse"), filepath.Join(tr.root, "evil")); err != nil {
		t.Fatal(err)
	}
	var evil *runtimemodule.Result
	for _, r := range tr.loader().LoadAll() {
		if r.Engine == "evil" {
			r := r
			evil = &r
		}
	}
	if evil == nil || !errors.Is(evil.Err, runtimemodule.ErrUntrusted) {
		t.Errorf("symlinked engine directory = %+v", evil)
	}
}

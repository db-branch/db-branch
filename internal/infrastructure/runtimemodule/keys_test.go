package runtimemodule_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

func newTestKeyStore(t *testing.T) (runtimemodule.KeyStore, string, *int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "trusted-keys")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	chownCalls := 0
	ks := runtimemodule.KeyStore{
		Dir:   dir,
		Owner: func(fs.FileInfo) (uint32, bool) { return 0, true },
		Chown: func(name string, uid, gid int) error {
			if uid != 0 || gid != 0 {
				t.Errorf("Chown(%q, %d, %d) want 0, 0", name, uid, gid)
			}
			chownCalls++
			return nil
		},
	}
	return ks, dir, &chownCalls
}

func TestKeyStore_信頼鍵を登録して一覧と取得ができ既存key_idの上書きを拒否する(t *testing.T) {
	if got := (runtimemodule.KeyStore{}).KeysDir(); got != runtimemodule.DefaultKeysDir {
		t.Fatalf("default KeysDir() = %q, want %q", got, runtimemodule.DefaultKeysDir)
	}

	ks, dir, chownCalls := newTestKeyStore(t)
	pub1, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wantFP, err := runtimemodule.KeyFingerprint(pub1)
	if err != nil {
		t.Fatal(err)
	}

	added, err := ks.Add("acme-2026", pub1)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.KeyID != "acme-2026" || !added.Fingerprint.Equal(wantFP) || !bytes.Equal(added.PublicKey, pub1) {
		t.Errorf("added = %+v", added)
	}
	if *chownCalls == 0 {
		t.Errorf("expected Chown to be called")
	}

	// File mode must be 0644 and no leftover temp files.
	fi, err := os.Stat(filepath.Join(dir, "acme-2026.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %04o, want 0644", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "acme-2026.pub" {
		t.Errorf("unexpected entries after Add: %v", entries)
	}

	// Get returns the stored key; unknown key returns ErrNotFound.
	got, err := ks.Get("acme-2026")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Fingerprint.Equal(wantFP) || !bytes.Equal(got.PublicKey, pub1) {
		t.Errorf("Get = %+v", got)
	}
	if _, err := ks.Get("missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}

	// Duplicate Add must fail with ErrKeyExists and never overwrite the existing key.
	if _, err := ks.Add("acme-2026", pub2); !errors.Is(err, domain.ErrKeyExists) {
		t.Errorf("duplicate Add = %v, want ErrKeyExists", err)
	}
	still, err := ks.Get("acme-2026")
	if err != nil || !bytes.Equal(still.PublicKey, pub1) {
		t.Errorf("existing key was altered: %+v, %v", still, err)
	}

	// Invalid public key length is refused.
	if _, err := ks.Add("short-key", ed25519.PublicKey([]byte("short"))); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("short pub Add = %v, want ErrInvalidRef", err)
	}

	// List returns valid keys and reports broken keys with Err, skipping non-.pub and dot files.
	write(t, filepath.Join(dir, "broken.pub"), []byte("!!!not-base64!!!"), 0o644)
	write(t, filepath.Join(dir, "writable.pub"), []byte("AAAA"), 0o666)
	write(t, filepath.Join(dir, ".tmp-ignore.pub"), []byte("bad"), 0o600)
	write(t, filepath.Join(dir, "README"), []byte("note"), 0o644)

	list, err := ks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3: %+v", len(list), list)
	}
	if list[0].KeyID != "acme-2026" || list[0].Err != nil || !list[0].Fingerprint.Equal(wantFP) {
		t.Errorf("list[0] = %+v", list[0])
	}
	if list[1].KeyID != "broken" || !errors.Is(list[1].Err, runtimemodule.ErrBadSignature) {
		t.Errorf("list[1] = %+v, want ErrBadSignature", list[1])
	}
	if list[2].KeyID != "writable" || !errors.Is(list[2].Err, runtimemodule.ErrUntrusted) {
		t.Errorf("list[2] = %+v, want ErrUntrusted", list[2])
	}

	// Untrusted directory (world-writable) must be rejected.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Get("acme-2026"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Get on 0777 dir = %v, want ErrUntrusted", err)
	}
	if _, err := ks.List(); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("List on 0777 dir = %v, want ErrUntrusted", err)
	}
	if _, err := ks.Add("new-key", pub2); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Add on 0777 dir = %v, want ErrUntrusted", err)
	}
}

type failReader struct{ err error }

func (f failReader) Read([]byte) (int, error) { return 0, f.err }

func TestKeyStore_登録済み鍵を削除でき存在しない鍵や危険なkey_idを拒否する(t *testing.T) {
	ks, dir, _ := newTestKeyStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Add("acme-2026", pub); err != nil {
		t.Fatal(err)
	}
	if err := ks.Remove("acme-2026"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := ks.Get("acme-2026"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after Remove = %v, want ErrNotFound", err)
	}
	if err := ks.Remove("acme-2026"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Remove missing = %v, want ErrNotFound", err)
	}

	// Dangerous key_ids must be rejected across Get/Add/Remove.
	for _, bad := range []string{"../evil", "..", ".", "a/b", "", "bad key"} {
		if _, err := ks.Get(bad); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("Get(%q) = %v, want ErrInvalidRef", bad, err)
		}
		if _, err := ks.Add(bad, pub); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("Add(%q) = %v, want ErrInvalidRef", bad, err)
		}
		if err := ks.Remove(bad); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidRef", bad, err)
		}
	}

	// Symlink in keys dir must be refused by Remove.
	target := filepath.Join(t.TempDir(), "real.pub")
	write(t, target, []byte("x"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "sym-key.pub")); err != nil {
		t.Fatal(err)
	}
	if err := ks.Remove("sym-key"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Remove(symlink) = %v, want ErrUntrusted", err)
	}

	// Invalid key_id filename on disk is reported with Err in List.
	write(t, filepath.Join(dir, "bad key.pub"), []byte("x"), 0o644)
	list, err := ks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	foundBad := false
	for _, k := range list {
		if k.KeyID == "bad key" && errors.Is(k.Err, domain.ErrInvalidRef) {
			foundBad = true
		}
	}
	if !foundBad {
		t.Errorf("List did not report invalid filename: %+v", list)
	}

	// Missing keys directory returns empty list without error.
	missingKS := runtimemodule.KeyStore{
		Dir:   filepath.Join(t.TempDir(), "no-such-dir"),
		Owner: func(fs.FileInfo) (uint32, bool) { return 0, true },
	}
	if empty, err := missingKS.List(); err != nil || len(empty) != 0 {
		t.Errorf("List on missing dir = %+v, %v", empty, err)
	}

	// Chown / Rand failure in Add leaves no temp file and returns error.
	ksErr := ks
	ksErr.Rand = failReader{err: errors.New("no entropy")}
	if _, err := ksErr.Add("k1", pub); err == nil {
		t.Errorf("Add with failing Rand succeeded")
	}
	ksChown := ks
	ksChown.Chown = func(string, int, int) error { return errors.New("chown denied") }
	if _, err := ksChown.Add("k2", pub); err == nil {
		t.Errorf("Add with failing Chown succeeded")
	}
	if _, err := ks.Get("k2"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("k2 should not exist after Chown failure: %v", err)
	}

	// Untrusted dir is rejected by Remove.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ks.Remove("acme-2026"); !errors.Is(err, runtimemodule.ErrUntrusted) {
		t.Errorf("Remove on 0777 dir = %v, want ErrUntrusted", err)
	}

	// Default Chown (nil) exercises real os.Chown path on non-root (fails with permission error).
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ksDefaultChown := ks
		ksDefaultChown.Chown = nil
		if _, err := ksDefaultChown.Add("default-chown", pub); err == nil {
			t.Errorf("Add with real os.Chown as non-root unexpectedly succeeded")
		}
	}
}

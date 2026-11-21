package runtimemodule_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

func TestKeyFingerprint_公開鍵のデコード済みバイト列から決定的なSHA256指紋を計算する(t *testing.T) {
	if runtimemodule.DefaultKeysDir != "/etc/db-branch/trusted-keys" {
		t.Fatalf("DefaultKeysDir = %q", runtimemodule.DefaultKeysDir)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(pub)

	p1, err := runtimemodule.ParsePublicKey([]byte(b64))
	if err != nil {
		t.Fatalf("ParsePublicKey without newline: %v", err)
	}
	p2, err := runtimemodule.ParsePublicKey([]byte(b64 + "\r\n"))
	if err != nil {
		t.Fatalf("ParsePublicKey with CRLF: %v", err)
	}

	fp1, err := runtimemodule.KeyFingerprint(p1)
	if err != nil {
		t.Fatalf("KeyFingerprint(p1): %v", err)
	}
	fp2, err := runtimemodule.KeyFingerprint(p2)
	if err != nil {
		t.Fatalf("KeyFingerprint(p2): %v", err)
	}
	sum := sha256.Sum256(pub)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if fp1.String() != want || !fp1.Equal(fp2) {
		t.Errorf("fp1=%q fp2=%q want=%q", fp1, fp2, want)
	}

	for name, bad := range map[string][]byte{
		"空":         nil,
		"壊れたbase64": []byte("!!!not-base64!!!"),
		"32バイト未満":   []byte(base64.StdEncoding.EncodeToString([]byte("short"))),
		"32バイト超過":   []byte(base64.StdEncoding.EncodeToString(make([]byte, 33))),
	} {
		if _, err := runtimemodule.ParsePublicKey(bad); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("ParsePublicKey(%s) = %v, want ErrBadSignature", name, err)
		}
	}
	if _, err := runtimemodule.KeyFingerprint(ed25519.PublicKey([]byte("short"))); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("KeyFingerprint(short) = %v, want ErrInvalidRef", err)
	}

	for _, id := range []string{"vendor-2026", "acme_1.0", "A-Z_0.9"} {
		if !runtimemodule.KeyIDValid(id) {
			t.Errorf("KeyIDValid(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../evil", "a/b", "a..b", "a b", "key\n", string(make([]byte, 65))} {
		if runtimemodule.KeyIDValid(id) {
			t.Errorf("KeyIDValid(%q) = true, want false", id)
		}
	}
}

func TestDecodeSignature_署名JSONからkey_idと署名バイト列を厳密に取り出す(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"engine":"clickhouse"}`)
	sig := ed25519.Sign(priv, payload)
	good, _ := json.Marshal(map[string]string{
		"key_id":    "acme-2026",
		"signature": base64.StdEncoding.EncodeToString(sig),
	})

	got, err := runtimemodule.DecodeSignature(good)
	if err != nil {
		t.Fatalf("DecodeSignature(good): %v", err)
	}
	if got.KeyID != "acme-2026" || !bytes.Equal(got.Bytes, sig) {
		t.Errorf("got = %+v", got)
	}
	if err := runtimemodule.VerifyWithKey(pub, payload, got.Bytes); err != nil {
		t.Errorf("VerifyWithKey(valid): %v", err)
	}
	if err := runtimemodule.VerifyWithKey(pub, []byte("tampered"), got.Bytes); !errors.Is(err, runtimemodule.ErrBadSignature) {
		t.Errorf("VerifyWithKey(tampered) = %v, want ErrBadSignature", err)
	}
	if err := runtimemodule.VerifyWithKey(ed25519.PublicKey([]byte("short")), payload, got.Bytes); !errors.Is(err, runtimemodule.ErrBadSignature) {
		t.Errorf("VerifyWithKey(short pub) = %v, want ErrBadSignature", err)
	}

	b64Sig := base64.StdEncoding.EncodeToString(sig)
	for name, raw := range map[string]string{
		"壊れたJSON":   "{",
		"未知フィールド":   `{"key_id":"acme-2026","signature":"` + b64Sig + `","extra":1}`,
		"後続ゴミ":      string(good) + `{"x":1}`,
		"不正key_id":  `{"key_id":"../evil","signature":"` + b64Sig + `"}`,
		"空key_id":   `{"key_id":"","signature":"` + b64Sig + `"}`,
		"壊れたbase64": `{"key_id":"acme-2026","signature":"!!!"}`,
		"長さ不正署名":    `{"key_id":"acme-2026","signature":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`,
	} {
		if _, err := runtimemodule.DecodeSignature([]byte(raw)); !errors.Is(err, runtimemodule.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func writeStagedModule(t *testing.T, tr *tree, stageDir, version string, binData, catData []byte, runAsUser string) {
	t.Helper()
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(stageDir, "db-branch-rt-clickhouse"), binData, 0o755)
	write(t, filepath.Join(stageDir, "runtimes.json"), catData, 0o644)
	m := domain.RuntimeManifest{
		SchemaVersion: 1, Engine: "clickhouse", DisplayName: "ClickHouse", ModuleVersion: version, APIVersion: 1,
		Binary: tr.binary, BinarySHA256: sum(binData), Catalog: tr.catalog, CatalogSHA256: sum(catData),
		RunAsUser:    runAsUser,
		Protocols:    []domain.ManifestPort{{Name: "http", Primary: true}},
		Capabilities: map[string]bool{"query": true},
		Systemd:      domain.ManifestSystemd{ServiceType: "simple", KillSignal: "SIGTERM", TimeoutStopSec: 300},
	}
	manBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(stageDir, "manifest.json"), manBytes, 0o644)
	sig := ed25519.Sign(tr.priv, manBytes)
	sigJSON, _ := json.Marshal(map[string]string{
		"key_id":    "vendor-2026",
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	write(t, filepath.Join(stageDir, "manifest.json.sig"), sigJSON, 0o644)
}

func TestLoaderVerifyDir_ステージング上のファイルを本番パス契約と署名で検証できる(t *testing.T) {
	tr := newTree(t)
	stageDir := filepath.Join(tr.root, ".staging-abc123")
	binV2 := []byte("#!/bin/sh\necho v2\n")
	catV2 := []byte(`[{"version":"24.1"}]`)
	writeStagedModule(t, tr, stageDir, "0.2.0", binV2, catV2, "db-branch-rt-clickhouse")

	got, err := tr.loader().VerifyDir(stageDir, "clickhouse", true)
	if err != nil {
		t.Fatalf("VerifyDir(stageDir): %v", err)
	}
	if !got.Signed || got.KeyID != "vendor-2026" || got.Manifest.ModuleVersion != "0.2.0" {
		t.Errorf("got = %+v", got)
	}

	// Production files are tampered/different, but staging verification must only check stageDir.
	write(t, tr.binary, []byte("#!/bin/sh\necho tampered-prod\n"), 0o755)
	if _, err := tr.loader().VerifyDir(stageDir, "clickhouse", true); err != nil {
		t.Errorf("VerifyDir read production binary instead of staged: %v", err)
	}

	// Tampering staged binary must be caught even if production binary matched something else.
	write(t, filepath.Join(stageDir, "db-branch-rt-clickhouse"), []byte("#!/bin/sh\necho bad\n"), 0o755)
	if _, err := tr.loader().VerifyDir(stageDir, "clickhouse", true); !errors.Is(err, runtimemodule.ErrDigestMismatch) {
		t.Errorf("tampered staged binary = %v, want ErrDigestMismatch", err)
	}
}

func TestLoaderVerifyDir_厳格モードでは生JSONのrun_as_user省略と無署名を拒否する(t *testing.T) {
	tr := newTree(t)
	stageDir := filepath.Join(tr.root, ".staging-strict")
	writeStagedModule(t, tr, stageDir, "0.2.0", []byte("#!/bin/sh\nexit 0\n"), []byte(`[]`), "")

	if _, err := tr.loader().VerifyDir(stageDir, "clickhouse", true); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("strict mode with empty run_as_user = %v, want ErrInvalidManifest", err)
	}
	if _, err := tr.loader().VerifyDir(stageDir, "clickhouse", false); err != nil {
		t.Errorf("non-strict mode with empty run_as_user = %v, want nil", err)
	}

	// Strict mode must reject unsigned even when AllowUnsigned is set.
	stageUnsigned := filepath.Join(tr.root, ".staging-unsigned")
	writeStagedModule(t, tr, stageUnsigned, "0.2.0", []byte("#!/bin/sh\nexit 0\n"), []byte(`[]`), "db-branch-rt-clickhouse")
	if err := os.Remove(filepath.Join(stageUnsigned, "manifest.json.sig")); err != nil {
		t.Fatal(err)
	}
	l := tr.loader()
	l.AllowUnsigned = true
	if _, err := l.VerifyDir(stageUnsigned, "clickhouse", true); !errors.Is(err, runtimemodule.ErrUnsigned) {
		t.Errorf("strict mode unsigned = %v, want ErrUnsigned", err)
	}
}

func TestLoadAll_ドット始まりの作業ディレクトリやロックファイルを読み飛ばす(t *testing.T) {
	tr := newTree(t)
	write(t, filepath.Join(tr.root, ".staging-xyz", "manifest.json"), []byte("{"), 0o644)
	write(t, filepath.Join(tr.root, ".old-123", "manifest.json"), []byte("{"), 0o644)
	write(t, filepath.Join(tr.root, ".lock"), nil, 0o600)

	got := tr.loader().LoadAll()
	if len(got) != 1 || got[0].Engine != "clickhouse" || got[0].Err != nil {
		t.Fatalf("LoadAll() = %+v, want only [clickhouse]", got)
	}
}

package rtmodule_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

// --- Fake ports for unit testing ---

type fakeReleaseSource struct {
	release     rtmodule.Release
	getErr      error
	payload     []byte
	downloadErr error
	gotRef      domain.GitHubRef
	gotAsset    rtmodule.ReleaseAsset
	gotMaxBytes int64
	getCalls    int
	dlCalls     int
}

func (f *fakeReleaseSource) GetRelease(_ context.Context, ref domain.GitHubRef) (rtmodule.Release, error) {
	f.getCalls++
	f.gotRef = ref
	if f.getErr != nil {
		return rtmodule.Release{}, f.getErr
	}
	return f.release, nil
}

func (f *fakeReleaseSource) DownloadAsset(_ context.Context, asset rtmodule.ReleaseAsset, maxBytes int64, dst io.Writer) (int64, error) {
	f.dlCalls++
	f.gotAsset = asset
	f.gotMaxBytes = maxBytes
	if f.downloadErr != nil {
		return 0, f.downloadErr
	}
	n, err := io.Copy(dst, bytes.NewReader(f.payload))
	return n, err
}

type fakeExtractor struct {
	mod           *rtmodule.ExtractedModule
	err           error
	calls         int
	gotArchive    string
	gotEngine     string
	cleanedUp     bool
	writtenSource *rtmodule.ModuleSource
	writeSrcErr   error
}

func (f *fakeExtractor) ExtractToStaging(_ context.Context, archivePath, engine string) (*rtmodule.ExtractedModule, error) {
	f.calls++
	f.gotArchive = archivePath
	f.gotEngine = engine
	if f.err != nil {
		return nil, f.err
	}
	if f.mod == nil {
		return nil, nil
	}
	m := *f.mod
	if m.Cleanup == nil {
		m.Cleanup = func() { f.cleanedUp = true }
	}
	if m.WriteSource == nil {
		m.WriteSource = func(src rtmodule.ModuleSource) error {
			if f.writeSrcErr != nil {
				return f.writeSrcErr
			}
			cp := src
			f.writtenSource = &cp
			return nil
		}
	}
	return &m, nil
}

type fakeKeyStore struct {
	keys       map[string]rtmodule.TrustKey
	getErr     error
	listErr    error
	addErr     error
	removeErr  error
	addCalls   int
	remCalls   int
	addedIDs   []string
	removedIDs []string
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{keys: map[string]rtmodule.TrustKey{}}
}

func (f *fakeKeyStore) Get(keyID string) (rtmodule.TrustKey, error) {
	if f.getErr != nil {
		return rtmodule.TrustKey{}, f.getErr
	}
	k, ok := f.keys[keyID]
	if !ok {
		return rtmodule.TrustKey{}, fmt.Errorf("%w: %s", domain.ErrNotFound, keyID)
	}
	return k, nil
}

func (f *fakeKeyStore) List() ([]rtmodule.TrustKey, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []rtmodule.TrustKey
	for _, k := range f.keys {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeKeyStore) Add(keyID string, pub ed25519.PublicKey) (rtmodule.TrustKey, error) {
	f.addCalls++
	if f.addErr != nil {
		return rtmodule.TrustKey{}, f.addErr
	}
	if _, exists := f.keys[keyID]; exists {
		return rtmodule.TrustKey{}, fmt.Errorf("%w: %s", domain.ErrKeyExists, keyID)
	}
	fp, err := domain.NewFingerprint(pub)
	if err != nil {
		return rtmodule.TrustKey{}, err
	}
	tk := rtmodule.TrustKey{
		KeyID:       keyID,
		Fingerprint: fp,
		PublicKey:   append(ed25519.PublicKey(nil), pub...),
	}
	f.keys[keyID] = tk
	f.addedIDs = append(f.addedIDs, keyID)
	return tk, nil
}

func (f *fakeKeyStore) Remove(keyID string) error {
	f.remCalls++
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, exists := f.keys[keyID]; !exists {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, keyID)
	}
	delete(f.keys, keyID)
	f.removedIDs = append(f.removedIDs, keyID)
	return nil
}

type fakeVerifier struct {
	verified     rtmodule.VerifiedModule
	verifyErr    error
	verifyCalls  int
	parseSigErr  error
	parsePubErr  error
	verifyKeyErr error
}

func (f *fakeVerifier) ParseSignature(sigRaw []byte) (string, []byte, error) {
	if f.parseSigErr != nil {
		return "", nil, f.parseSigErr
	}
	var raw struct {
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(sigRaw, &raw); err != nil {
		return "", nil, fmt.Errorf("%w: %v", rtmodule.ErrBadSignature, err)
	}
	sig, err := base64.StdEncoding.DecodeString(raw.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", nil, fmt.Errorf("%w: invalid signature bytes", rtmodule.ErrBadSignature)
	}
	return raw.KeyID, sig, nil
}

func (f *fakeVerifier) ParsePublicKey(keyRaw []byte) (ed25519.PublicKey, domain.Fingerprint, error) {
	if f.parsePubErr != nil {
		return nil, domain.Fingerprint{}, f.parsePubErr
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(keyRaw), "\r\n"))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, domain.Fingerprint{}, fmt.Errorf("%w: invalid public key", rtmodule.ErrBadSignature)
	}
	fp, err := domain.NewFingerprint(b)
	if err != nil {
		return nil, domain.Fingerprint{}, err
	}
	return ed25519.PublicKey(b), fp, nil
}

func (f *fakeVerifier) VerifyWithKey(pub ed25519.PublicKey, manifestRaw, sig []byte) error {
	if f.verifyKeyErr != nil {
		return f.verifyKeyErr
	}
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, manifestRaw, sig) {
		return fmt.Errorf("%w: verification failed", rtmodule.ErrBadSignature)
	}
	return nil
}

func (f *fakeVerifier) VerifyStaging(_, _ string) (rtmodule.VerifiedModule, error) {
	f.verifyCalls++
	if f.verifyErr != nil {
		return rtmodule.VerifiedModule{}, f.verifyErr
	}
	return f.verified, nil
}

type fakeModuleStore struct {
	modules     map[string]rtmodule.VerifiedModule
	sources     map[string]rtmodule.ModuleSource
	listItems   []rtmodule.VerifiedModule
	lockErr     error
	getErr      error
	readSrcErr  error
	commitErr   error
	removeErr   error
	lockCalls   int
	unlockCalls int
	commitCalls int
	removeCalls int
	committed   struct {
		stageDir string
		engine   string
		upgrade  bool
	}
	removedEngine string
	events        *[]string
}

func newFakeModuleStore() *fakeModuleStore {
	return &fakeModuleStore{
		modules: map[string]rtmodule.VerifiedModule{},
		sources: map[string]rtmodule.ModuleSource{},
	}
}

func (f *fakeModuleStore) record(ev string) {
	if f.events != nil {
		*f.events = append(*f.events, ev)
	}
}

func (f *fakeModuleStore) Lock(_ context.Context) (func(), error) {
	f.lockCalls++
	f.record("lock")
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return func() {
		f.unlockCalls++
		f.record("unlock")
	}, nil
}

func (f *fakeModuleStore) Get(engine string) (rtmodule.VerifiedModule, error) {
	if f.getErr != nil {
		return rtmodule.VerifiedModule{}, f.getErr
	}
	m, ok := f.modules[engine]
	if !ok {
		return rtmodule.VerifiedModule{}, fmt.Errorf("%w: %s", domain.ErrNotFound, engine)
	}
	return m, nil
}

func (f *fakeModuleStore) List() []rtmodule.VerifiedModule {
	if f.listItems != nil {
		return f.listItems
	}
	var out []rtmodule.VerifiedModule
	for _, m := range f.modules {
		out = append(out, m)
	}
	return out
}

func (f *fakeModuleStore) Commit(stageDir, engine string, upgrade bool) error {
	f.commitCalls++
	f.record("commit")
	f.committed.stageDir = stageDir
	f.committed.engine = engine
	f.committed.upgrade = upgrade
	return f.commitErr
}

func (f *fakeModuleStore) Remove(engine string) error {
	f.removeCalls++
	f.removedEngine = engine
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.modules, engine)
	return nil
}

func (f *fakeModuleStore) ReadSource(engine string) (rtmodule.ModuleSource, error) {
	if f.readSrcErr != nil {
		return rtmodule.ModuleSource{}, f.readSrcErr
	}
	src, ok := f.sources[engine]
	if !ok {
		return rtmodule.ModuleSource{}, fmt.Errorf("%w: %s", domain.ErrNotFound, engine)
	}
	return src, nil
}

type fakeSystemUsers struct {
	created bool
	uid     uint32
	gid     uint32
	err     error
	calls   int
	gotUser string
	events  *[]string
}

func (f *fakeSystemUsers) EnsureSystemUser(_ context.Context, username string) (bool, uint32, uint32, error) {
	f.calls++
	f.gotUser = username
	if f.events != nil {
		*f.events = append(*f.events, "user")
	}
	if f.err != nil {
		return false, 0, 0, f.err
	}
	return f.created, f.uid, f.gid, nil
}

type fakeUsageChecker struct {
	bases    []string
	branches []string
	err      error
	calls    int
	gotEng   domain.Engine
}

func (f *fakeUsageChecker) InUse(_ context.Context, engine domain.Engine) ([]string, []string, error) {
	f.calls++
	f.gotEng = engine
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.bases, f.branches, nil
}

type fakePrompter struct {
	approve bool
	err     error
	calls   int
	gotID   string
	gotFP   domain.Fingerprint
	gotRef  domain.GitHubRef
	gotTag  string
}

func (f *fakePrompter) ConfirmTrust(_ context.Context, keyID string, fp domain.Fingerprint, ref domain.GitHubRef, tag string) (bool, error) {
	f.calls++
	f.gotID = keyID
	f.gotFP = fp
	f.gotRef = ref
	f.gotTag = tag
	return f.approve, f.err
}

type fakeRestarter struct {
	err    error
	calls  int
	events *[]string
}

func (f *fakeRestarter) Restart(_ context.Context) error {
	f.calls++
	if f.events != nil {
		*f.events = append(*f.events, "restart")
	}
	return f.err
}

// --- Test helpers ---

type testKeyPair struct {
	keyID string
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	fp    domain.Fingerprint
	raw   []byte
}

func newTestKeyPair(t *testing.T, keyID string) testKeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := domain.NewFingerprint(pub)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
	return testKeyPair{keyID: keyID, pub: pub, priv: priv, fp: fp, raw: raw}
}

func (k testKeyPair) sign(t *testing.T, data []byte) []byte {
	t.Helper()
	sig := ed25519.Sign(k.priv, data)
	payload, err := json.Marshal(map[string]string{
		"key_id":    k.keyID,
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func makeManifest(engine, version, runAsUser string) (domain.RuntimeManifest, []byte) {
	m := domain.RuntimeManifest{
		SchemaVersion: 1,
		Engine:        engine,
		DisplayName:   strings.ToUpper(engine),
		ModuleVersion: version,
		APIVersion:    1,
		Binary:        "/usr/lib/db-branch/runtimes/" + engine + "/db-branch-rt-" + engine,
		BinarySHA256:  strings.Repeat("a", 64),
		Catalog:       "/usr/lib/db-branch/runtimes/" + engine + "/runtimes.json",
		CatalogSHA256: strings.Repeat("b", 64),
		RunAsUser:     runAsUser,
		Protocols:     []domain.ManifestPort{{Name: "http", Primary: true}},
	}
	raw, _ := json.Marshal(m)
	return m, raw
}

type testHarness struct {
	svc       *rtmodule.Service
	releases  *fakeReleaseSource
	extractor *fakeExtractor
	keys      *fakeKeyStore
	verifier  *fakeVerifier
	store     *fakeModuleStore
	users     *fakeSystemUsers
	usage     *fakeUsageChecker
	prompter  *fakePrompter
	restarter *fakeRestarter
	tmpDir    string
	events    []string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	h := &testHarness{
		releases:  &fakeReleaseSource{payload: []byte("fake-tar-gz")},
		extractor: &fakeExtractor{},
		keys:      newFakeKeyStore(),
		verifier:  &fakeVerifier{},
		store:     newFakeModuleStore(),
		users:     &fakeSystemUsers{created: true, uid: 984, gid: 984},
		usage:     &fakeUsageChecker{},
		prompter:  &fakePrompter{approve: true},
		restarter: &fakeRestarter{},
		tmpDir:    t.TempDir(),
	}
	h.store.events = &h.events
	h.users.events = &h.events
	h.restarter.events = &h.events

	h.svc = &rtmodule.Service{
		Releases:  h.releases,
		Extractor: h.extractor,
		Keys:      h.keys,
		Verifier:  h.verifier,
		Store:     h.store,
		Users:     h.users,
		Usage:     h.usage,
		Prompter:  h.prompter,
		Restarter: h.restarter,
		Arch:      "amd64",
		CreateTemp: func() (rtmodule.TempFile, error) {
			return os.CreateTemp(h.tmpDir, "dl-*.tar.gz")
		},
		RemoveFile: os.Remove,
	}
	return h
}

func (h *testHarness) setupValidRelease(t *testing.T, engine, version, tag string, kp testKeyPair, includeCandidate bool) {
	t.Helper()
	manifest, manifestRaw := makeManifest(engine, version, domain.RuntimeUserPrefix+engine)
	sigRaw := kp.sign(t, manifestRaw)

	h.releases.release = rtmodule.Release{
		Tag: tag,
		Assets: []rtmodule.ReleaseAsset{
			{Name: "SHA256SUMS", DownloadURL: "https://github.com/acme/repo/releases/download/" + tag + "/SHA256SUMS"},
			{Name: "db-branch-rt-" + engine + "_" + version + "_linux_amd64.tar.gz", DownloadURL: "https://github.com/acme/repo/releases/download/" + tag + "/amd64.tar.gz", Size: 1234},
			{Name: "db-branch-rt-" + engine + "_" + version + "_linux_arm64.tar.gz", DownloadURL: "https://github.com/acme/repo/releases/download/" + tag + "/arm64.tar.gz", Size: 1234},
		},
	}
	mod := &rtmodule.ExtractedModule{
		StageDir:     filepath.Join(h.tmpDir, ".staging-abc"),
		ManifestRaw:  manifestRaw,
		SignatureRaw: sigRaw,
	}
	if includeCandidate {
		mod.CandidateKeyID = kp.keyID
		mod.CandidateKeyRaw = kp.raw
	}
	h.extractor.mod = mod
	h.verifier.verified = rtmodule.VerifiedModule{
		Engine:   engine,
		Manifest: manifest,
		KeyID:    kp.keyID,
		Signed:   true,
	}
}

// Test 25
func TestInstall_登録済みの信頼鍵で署名されたReleaseを導入しOSユーザー作成と再起動を行う(t *testing.T) {
	h := newTestHarness(t)
	kp := newTestKeyPair(t, "acme-2026")
	if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
		t.Fatal(err)
	}
	h.keys.addCalls = 0
	h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)

	ref, err := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	if res.Engine != "clickhouse" || res.Version != "0.2.0" || res.Tag != "v0.2.0" {
		t.Errorf("unexpected result identity: %+v", res)
	}
	if res.KeyID != "acme-2026" || !res.KeyFingerprint.Equal(kp.fp) {
		t.Errorf("unexpected key info: %+v", res)
	}
	if res.Upgraded || !res.UserCreated || res.KeyAdded || !res.Installed || !res.Restarted || res.RestartFailed {
		t.Errorf("unexpected result flags: %+v", res)
	}
	if res.User != "db-branch-rt-clickhouse" || res.UserUID != 984 || res.UserGID != 984 {
		t.Errorf("unexpected user info: %+v", res)
	}
	if h.prompter.calls != 0 || h.keys.addCalls != 0 {
		t.Errorf("expected no prompt/key add for trusted key, got prompter=%d add=%d", h.prompter.calls, h.keys.addCalls)
	}
	if h.releases.gotMaxBytes != rtmodule.DefaultMaxDownloadBytes {
		t.Errorf("expected maxBytes=%d, got %d", rtmodule.DefaultMaxDownloadBytes, h.releases.gotMaxBytes)
	}
	if h.extractor.writtenSource == nil || *h.extractor.writtenSource != (rtmodule.ModuleSource{
		Repo:  "github.com/acme/db-branch-rt-clickhouse",
		Tag:   "v0.2.0",
		KeyID: "acme-2026",
	}) {
		t.Errorf("unexpected written source: %+v", h.extractor.writtenSource)
	}
	if !h.extractor.cleanedUp {
		t.Errorf("expected stage cleanup to be called")
	}
	if h.store.committed.engine != "clickhouse" || h.store.committed.upgrade {
		t.Errorf("unexpected commit args: %+v", h.store.committed)
	}
	// Verify ordering: lock -> user -> commit -> restart -> unlock
	wantOrder := []string{"lock", "user", "commit", "restart", "unlock"}
	if strings.Join(h.events, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("unexpected event order: got %v, want %v", h.events, wantOrder)
	}
	// Verify temporary archive file was cleaned up
	entries, err := os.ReadDir(h.tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp archive to be deleted, found %d files", len(entries))
	}
}

// Test 26
func TestInstall_未登録鍵は対話承認または一致するtrust_keyでのみ登録して進みそれ以外は拒否する(t *testing.T) {
	ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

	t.Run("対話承認yで鍵を登録して導入に成功する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.prompter.approve = true

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if err != nil {
			t.Fatalf("Install failed: %v", err)
		}
		if !res.KeyAdded || res.KeyID != "acme-2026" || !res.KeyFingerprint.Equal(kp.fp) {
			t.Errorf("unexpected key result: %+v", res)
		}
		if h.prompter.calls != 1 || h.prompter.gotID != "acme-2026" || !h.prompter.gotFP.Equal(kp.fp) || h.prompter.gotTag != "v0.2.0" {
			t.Errorf("unexpected prompter invocation: %+v", h.prompter)
		}
		if _, err := h.keys.Get("acme-2026"); err != nil {
			t.Errorf("expected key to be stored: %v", err)
		}
	})

	t.Run("対話拒否NでErrKeyUntrustedとなり鍵もユーザーも配置も残らない", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.prompter.approve = false

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, domain.ErrKeyUntrusted) {
			t.Fatalf("want ErrKeyUntrusted, got %v", err)
		}
		if h.keys.addCalls != 0 || h.users.calls != 0 || h.store.commitCalls != 0 {
			t.Errorf("expected no side effects: add=%d users=%d commit=%d", h.keys.addCalls, h.users.calls, h.store.commitCalls)
		}
		if !h.extractor.cleanedUp {
			t.Errorf("expected staging cleanup on decline")
		}
	})

	t.Run("Prompterがnilの非対話環境でtrust_key未指定ならErrKeyUntrusted", func(t *testing.T) {
		h := newTestHarness(t)
		h.svc.Prompter = nil
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, domain.ErrKeyUntrusted) {
			t.Fatalf("want ErrKeyUntrusted, got %v", err)
		}
		if h.keys.addCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("expected no key add and cleanup")
		}
	})

	t.Run("Prompterがエラーを返したらそのまま失敗し鍵登録しない", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.prompter.err = fmt.Errorf("%w: non-interactive stdin", domain.ErrKeyUntrusted)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, domain.ErrKeyUntrusted) {
			t.Fatalf("want ErrKeyUntrusted, got %v", err)
		}
		if h.keys.addCalls != 0 {
			t.Errorf("expected no key add")
		}
	})

	t.Run("一致するtrust_keyがあれば対話なしで鍵登録し導入成功する", func(t *testing.T) {
		h := newTestHarness(t)
		h.svc.Prompter = nil
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref:      ref,
			TrustKey: kp.fp,
		})
		if err != nil {
			t.Fatalf("Install failed: %v", err)
		}
		if !res.KeyAdded || !res.Installed {
			t.Errorf("unexpected result: %+v", res)
		}
	})

	t.Run("trust_keyが不一致ならプロンプトを呼ばずErrKeyMismatchで拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		other := newTestKeyPair(t, "other-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref:      ref,
			TrustKey: other.fp,
		})
		if !errors.Is(err, domain.ErrKeyMismatch) {
			t.Fatalf("want ErrKeyMismatch, got %v", err)
		}
		if h.prompter.calls != 0 || h.keys.addCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("expected no prompt/add and cleanup: prompter=%d add=%d", h.prompter.calls, h.keys.addCalls)
		}
	})

	t.Run("同梱候補鍵が無いまたはkey_idが違う場合はErrKeyUntrustedで拒否する", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			candID string
			raw    []byte
		}{
			{"候補鍵なし", "", nil},
			{"key_id不一致", "different-id", []byte("unused")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newTestHarness(t)
				kp := newTestKeyPair(t, "acme-2026")
				h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
				h.extractor.mod.CandidateKeyID = tc.candID
				h.extractor.mod.CandidateKeyRaw = tc.raw

				_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
				if !errors.Is(err, domain.ErrKeyUntrusted) {
					t.Fatalf("want ErrKeyUntrusted, got %v", err)
				}
				if h.prompter.calls != 0 || h.keys.addCalls != 0 || !h.extractor.cleanedUp {
					t.Errorf("expected no prompt/add and cleanup")
				}
			})
		}
	})

	t.Run("候補鍵で署名が通らない場合はプロンプトを出す前にErrBadSignatureで拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		signer := newTestKeyPair(t, "acme-2026")
		wrongCand := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", signer, false)
		h.extractor.mod.CandidateKeyID = "acme-2026"
		h.extractor.mod.CandidateKeyRaw = wrongCand.raw

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, rtmodule.ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
		if h.prompter.calls != 0 || h.keys.addCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("candidate key with bad signature must never be prompted or added")
		}
	})

	t.Run("KeyStore.Addで競合ErrKeyExistsなら上書きせずそのまま失敗する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.keys.addErr = fmt.Errorf("%w: acme-2026", domain.ErrKeyExists)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, domain.ErrKeyExists) {
			t.Fatalf("want ErrKeyExists, got %v", err)
		}
		if h.keys.remCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("failed Add must not remove pre-existing key and must cleanup stage")
		}
	})

	t.Run("新規登録した鍵は後続ステップの失敗でRemoveにより取り消される", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.store.commitErr = errors.New("disk full")

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("expected commit error, got %v", err)
		}
		if h.keys.addCalls != 1 || h.keys.remCalls != 1 {
			t.Errorf("expected Add once and rollback Remove once, got add=%d rem=%d", h.keys.addCalls, h.keys.remCalls)
		}
		if _, gErr := h.keys.Get("acme-2026"); !errors.Is(gErr, domain.ErrNotFound) {
			t.Errorf("expected newly added key to be rolled back, got %v", gErr)
		}
		if !h.extractor.cleanedUp {
			t.Errorf("expected staging cleanup on commit failure")
		}
	})
}

// Test 27
func TestInstall_既知key_idの署名検証に失敗しても同梱公開鍵で上書きや再確認をしない(t *testing.T) {
	ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

	t.Run("登録済み鍵での署名検証失敗時に候補鍵を一切参照せずErrBadSignatureを返す", func(t *testing.T) {
		h := newTestHarness(t)
		trusted := newTestKeyPair(t, "acme-2026")
		attacker := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(trusted.keyID, trusted.pub); err != nil {
			t.Fatal(err)
		}
		h.keys.addCalls = 0
		// Release is signed by attacker's key AND bundles attacker's matching candidate key.
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", attacker, true)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref:      ref,
			TrustKey: attacker.fp,
		})
		if !errors.Is(err, rtmodule.ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
		if h.prompter.calls != 0 || h.keys.addCalls != 0 || h.keys.remCalls != 0 {
			t.Errorf("known key failure must never prompt, add, or remove keys: prompt=%d add=%d rem=%d",
				h.prompter.calls, h.keys.addCalls, h.keys.remCalls)
		}
		if h.users.calls != 0 || h.store.commitCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("expected abort and cleanup without side effects")
		}
	})

	t.Run("KeyStore.GetがErrNotFound以外のエラーを返した場合は未登録扱いにせず失敗する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.keys.getErr = errors.New("corrupt key file")

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if err == nil || !strings.Contains(err.Error(), "corrupt key file") {
			t.Fatalf("want corrupt key error, got %v", err)
		}
		if h.prompter.calls != 0 || h.keys.addCalls != 0 {
			t.Errorf("must not prompt or add when KeyStore.Get returns non-NotFound error")
		}
	})

	t.Run("登録済み鍵に対して明示指定されたtrust_keyが不一致ならErrKeyMismatchで拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		trusted := newTestKeyPair(t, "acme-2026")
		other := newTestKeyPair(t, "other-2026")
		if _, err := h.keys.Add(trusted.keyID, trusted.pub); err != nil {
			t.Fatal(err)
		}
		h.keys.addCalls = 0
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", trusted, false)

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref:      ref,
			TrustKey: other.fp,
		})
		if !errors.Is(err, domain.ErrKeyMismatch) {
			t.Fatalf("want ErrKeyMismatch, got %v", err)
		}
		if h.prompter.calls != 0 || h.keys.addCalls != 0 || h.store.commitCalls != 0 {
			t.Errorf("expected no side effects")
		}
	})
}

// Test 28
func TestInstall_manifestのrun_as_userやengineやversionが契約と不一致なら配置せず掃除する(t *testing.T) {
	ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

	setupTrusted := func(t *testing.T) (*testHarness, testKeyPair) {
		t.Helper()
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
			t.Fatal(err)
		}
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
		return h, kp
	}

	t.Run("run_as_userが空または不正値ならErrInvalidManifestで拒否し掃除する", func(t *testing.T) {
		cases := []struct {
			name         string
			manifestUser string
			rawJSONUser  string
		}{
			{"構造体とJSONともに空", "", ""},
			{"JSONで省略され構造体だけ補完されたケース", "db-branch-rt-clickhouse", ""},
			{"root指定", "root", "root"},
			{"他エンジン用ユーザー", "db-branch-rt-other", "db-branch-rt-other"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h, kp := setupTrusted(t)
				m, raw := makeManifest("clickhouse", "0.2.0", tc.rawJSONUser)
				m.RunAsUser = tc.manifestUser
				h.extractor.mod.ManifestRaw = raw
				h.extractor.mod.SignatureRaw = kp.sign(t, raw)
				h.verifier.verified.Manifest = m

				_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
				if !errors.Is(err, domain.ErrInvalidManifest) {
					t.Fatalf("want ErrInvalidManifest, got %v", err)
				}
				if !h.extractor.cleanedUp || h.users.calls != 0 || h.store.commitCalls != 0 {
					t.Errorf("must cleanup without creating user or committing")
				}
			})
		}
	})

	t.Run("組み込みエンジン名やassetとmanifestのengine不一致を拒否し掃除する", func(t *testing.T) {
		for _, badEngine := range []string{"mysql", "postgres", "duckdb"} {
			t.Run(badEngine, func(t *testing.T) {
				h, kp := setupTrusted(t)
				m, raw := makeManifest(badEngine, "0.2.0", domain.RuntimeUserPrefix+badEngine)
				h.extractor.mod.ManifestRaw = raw
				h.extractor.mod.SignatureRaw = kp.sign(t, raw)
				h.verifier.verified.Manifest = m

				_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
				if !errors.Is(err, domain.ErrInvalidManifest) {
					t.Fatalf("want ErrInvalidManifest for engine %q, got %v", badEngine, err)
				}
				if !h.extractor.cleanedUp || h.users.calls != 0 || h.store.commitCalls != 0 {
					t.Errorf("must cleanup without creating user or committing")
				}
			})
		}
	})

	t.Run("manifestのmodule_versionがassetとtagのversionに不一致なら拒否し掃除する", func(t *testing.T) {
		h, kp := setupTrusted(t)
		m, raw := makeManifest("clickhouse", "0.1.9", "db-branch-rt-clickhouse")
		h.extractor.mod.ManifestRaw = raw
		h.extractor.mod.SignatureRaw = kp.sign(t, raw)
		h.verifier.verified.Manifest = m

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if !errors.Is(err, domain.ErrInvalidManifest) {
			t.Fatalf("want ErrInvalidManifest, got %v", err)
		}
		if !h.extractor.cleanedUp || h.users.calls != 0 || h.store.commitCalls != 0 {
			t.Errorf("must cleanup without creating user or committing")
		}
	})

	t.Run("assetのversionとreleaseのtagが不一致なら拒否する", func(t *testing.T) {
		h, _ := setupTrusted(t)
		h.releases.release.Tag = "v0.3.0" // assets are 0.2.0

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref: domain.GitHubRef{Owner: "acme", Repo: "db-branch-rt-clickhouse"},
		})
		if err == nil {
			t.Fatal("expected error for tag/asset version mismatch")
		}
		if h.extractor.calls != 0 || h.users.calls != 0 || h.store.commitCalls != 0 {
			t.Errorf("must abort before extraction")
		}
	})

	t.Run("対象archのassetが0個または複数ある場合は拒否する", func(t *testing.T) {
		t.Run("0個", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Assets = []rtmodule.ReleaseAsset{
				{Name: "db-branch-rt-clickhouse_0.2.0_linux_arm64.tar.gz"},
				{Name: "README.md"},
			}
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil {
				t.Fatal("expected error for 0 matching assets")
			}
		})
		t.Run("複数一致", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Assets = []rtmodule.ReleaseAsset{
				{Name: "db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz"},
				{Name: "db-branch-rt-duckdb_0.2.0_linux_amd64.tar.gz"},
			}
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil {
				t.Fatal("expected error for multiple matching assets")
			}
		})
	})

	t.Run("draftやprereleaseや指定tagとの不一致を拒否する", func(t *testing.T) {
		t.Run("draft", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Draft = true
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil {
				t.Fatal("expected error for draft release")
			}
		})
		t.Run("prerelease", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Prerelease = true
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil {
				t.Fatal("expected error for prerelease")
			}
		})
		t.Run("要求tagと応答tagの不一致", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Tag = "v0.9.9"
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil {
				t.Fatal("expected error for response tag mismatch")
			}
		})
	})

	t.Run("不正なGitHubRefや未対応archを拒否する", func(t *testing.T) {
		h, _ := setupTrusted(t)
		if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
			Ref: domain.GitHubRef{Owner: "..", Repo: "bad"},
		}); !errors.Is(err, domain.ErrInvalidRef) {
			t.Fatalf("want ErrInvalidRef, got %v", err)
		}
		h.svc.Arch = "riscv64"
		if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, domain.ErrInvalidRef) {
			t.Fatalf("want ErrInvalidRef for bad arch, got %v", err)
		}
		if h.store.lockCalls != 0 {
			t.Errorf("invalid input should be rejected before taking lock")
		}
	})
}

// Test 29
func TestInstall_既存導入済みはupgradeなしで拒否しupgrade時でもダウングレードは拒否する(t *testing.T) {
	setupExisting := func(t *testing.T, existingVer, existingKeyID, existingRepo string, hasSource bool) (*testHarness, testKeyPair) {
		t.Helper()
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
			t.Fatal(err)
		}
		m, _ := makeManifest("clickhouse", existingVer, "db-branch-rt-clickhouse")
		h.store.modules["clickhouse"] = rtmodule.VerifiedModule{
			Engine:   "clickhouse",
			Manifest: m,
			KeyID:    existingKeyID,
			Signed:   existingKeyID != "",
		}
		if hasSource {
			h.store.sources["clickhouse"] = rtmodule.ModuleSource{
				Repo:  existingRepo,
				Tag:   "v" + existingVer,
				KeyID: existingKeyID,
			}
		}
		return h, kp
	}

	t.Run("既存0.2.0に対しupgradeなしならErrAlreadyInstalledで拒否する", func(t *testing.T) {
		h, kp := setupExisting(t, "0.2.0", "acme-2026", "github.com/acme/db-branch-rt-clickhouse", true)
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.3.0")

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: false})
		if !errors.Is(err, domain.ErrAlreadyInstalled) {
			t.Fatalf("want ErrAlreadyInstalled, got %v", err)
		}
		if h.users.calls != 0 || h.store.commitCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("must abort without committing and cleanup stage")
		}
	})

	t.Run("既存0.2.0に対しupgradeありで同一版や旧版ならErrDowngradeRefusedで拒否する", func(t *testing.T) {
		for _, ver := range []string{"0.2.0", "0.1.0"} {
			t.Run(ver, func(t *testing.T) {
				h, kp := setupExisting(t, "0.2.0", "acme-2026", "github.com/acme/db-branch-rt-clickhouse", true)
				h.setupValidRelease(t, "clickhouse", ver, "v"+ver, kp, false)
				ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v" + ver)

				_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
				if !errors.Is(err, domain.ErrDowngradeRefused) {
					t.Fatalf("want ErrDowngradeRefused for %s, got %v", ver, err)
				}
				if h.users.calls != 0 || h.store.commitCalls != 0 || !h.extractor.cleanedUp {
					t.Errorf("must abort without committing and cleanup stage")
				}
			})
		}
	})

	t.Run("既存0.2.0に対し同一repoからの0.3.0アップグレードに成功する", func(t *testing.T) {
		h, kp := setupExisting(t, "0.2.0", "acme-2026", "github.com/acme/db-branch-rt-clickhouse", true)
		h.users.created = false
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.3.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		if !res.Upgraded || res.PreviousVersion != "0.2.0" || res.Version != "0.3.0" || res.UserCreated || res.SourceMissing {
			t.Errorf("unexpected upgrade result: %+v", res)
		}
		if !h.store.committed.upgrade || h.store.committed.engine != "clickhouse" {
			t.Errorf("expected Commit with upgrade=true, got %+v", h.store.committed)
		}
	})

	t.Run("別owner_repoからのupgradeはErrSourceMismatchかつErrConflictで拒否する", func(t *testing.T) {
		h, kp := setupExisting(t, "0.2.0", "acme-2026", "github.com/acme/db-branch-rt-clickhouse", true)
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/evil/db-branch-rt-clickhouse@v0.3.0")

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
		if !errors.Is(err, rtmodule.ErrSourceMismatch) || !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("want ErrSourceMismatch+ErrConflict, got %v", err)
		}
		if h.users.calls != 0 || h.store.commitCalls != 0 || !h.extractor.cleanedUp {
			t.Errorf("must abort without committing and cleanup stage")
		}
	})

	t.Run("出所メタデータが無い既存モジュールからのupgradeは警告とSourceMissingを記録して続行する", func(t *testing.T) {
		h, kp := setupExisting(t, "0.2.0", "acme-2026", "", false)
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.3.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
		if err != nil {
			t.Fatalf("upgrade without provenance should succeed: %v", err)
		}
		if !res.Upgraded || !res.SourceMissing || len(res.Warnings) == 0 {
			t.Errorf("expected SourceMissing=true and warning, got %+v", res)
		}
	})

	t.Run("出所メタデータの読取がNotFound以外のエラーならupgradeを拒否する", func(t *testing.T) {
		h, kp := setupExisting(t, "0.2.0", "acme-2026", "", false)
		h.store.readSrcErr = errors.New("corrupt .source.json")
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.3.0")

		if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true}); err == nil || !strings.Contains(err.Error(), "corrupt .source.json") {
			t.Fatalf("expected corrupt provenance error, got %v", err)
		}
	})

	t.Run("未導入エンジンにupgrade指定するとErrNotFoundで拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
			t.Fatal(err)
		}
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

		_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		if h.users.calls != 0 || h.store.commitCalls != 0 {
			t.Errorf("must not create user or commit")
		}
	})

	t.Run("upgrade時に署名鍵のkey_idが変わった場合はPreviousKeyIDと警告を記録する", func(t *testing.T) {
		h, _ := setupExisting(t, "0.2.0", "acme-2025", "github.com/acme/db-branch-rt-clickhouse", true)
		newKP := newTestKeyPair(t, "acme-2027")
		if _, err := h.keys.Add(newKP.keyID, newKP.pub); err != nil {
			t.Fatal(err)
		}
		h.setupValidRelease(t, "clickhouse", "0.3.0", "v0.3.0", newKP, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.3.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, Upgrade: true})
		if err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		if res.PreviousKeyID != "acme-2025" || res.KeyID != "acme-2027" || len(res.Warnings) == 0 {
			t.Errorf("expected key change warning, got %+v", res)
		}
	})

	t.Run("NoRestart指定時は再起動しない", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		_, _ = h.keys.Add(kp.keyID, kp.pub)
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, NoRestart: true})
		if err != nil {
			t.Fatalf("Install failed: %v", err)
		}
		if res.Restarted || res.RestartFailed || h.restarter.calls != 0 {
			t.Errorf("expected no restart, got res=%+v calls=%d", res, h.restarter.calls)
		}
	})

	t.Run("Restart失敗時はエラーを返すが導入済み状態と新規登録鍵は戻さない", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
		h.restarter.err = errors.New("systemd unavailable")
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref, TrustKey: kp.fp})
		if err == nil || !strings.Contains(err.Error(), "systemd unavailable") {
			t.Fatalf("expected restart error, got %v", err)
		}
		if !res.Installed || !res.RestartFailed || res.Restarted || !res.KeyAdded {
			t.Errorf("expected Installed=true RestartFailed=true KeyAdded=true, got %+v", res)
		}
		if h.keys.remCalls != 0 {
			t.Errorf("newly added key must NOT be rolled back after successful Commit")
		}
	})
}

// Test 30
func TestRemove_baseまたはbranchが使用中なら拒否し未使用ならモジュールを削除してOSユーザーは残す(t *testing.T) {
	setupInstalled := func(t *testing.T) *testHarness {
		t.Helper()
		h := newTestHarness(t)
		m, _ := makeManifest("clickhouse", "0.2.0", "db-branch-rt-clickhouse")
		h.store.modules["clickhouse"] = rtmodule.VerifiedModule{
			Engine:   "clickhouse",
			Manifest: m,
			KeyID:    "acme-2026",
			Signed:   true,
		}
		return h
	}

	t.Run("組み込みエンジン名や不正なエンジン名はErrInvalidModuleで拒否する", func(t *testing.T) {
		for _, bad := range []string{"mysql", "postgres", "", "../evil"} {
			h := setupInstalled(t)
			_, err := h.svc.Remove(context.Background(), bad, false)
			if !errors.Is(err, domain.ErrInvalidModule) {
				t.Fatalf("engine %q: want ErrInvalidModule, got %v", bad, err)
			}
			if h.usage.calls != 0 || h.store.removeCalls != 0 {
				t.Errorf("engine %q: must reject before usage check or removal", bad)
			}
		}
	})

	t.Run("baseまたはbranchが使用中なら件数と名前を含むErrInUseで拒否し削除しない", func(t *testing.T) {
		cases := []struct {
			name     string
			bases    []string
			branches []string
			wantSub  []string
		}{
			{
				name:    "baseのみ使用中",
				bases:   []string{"analytics-base"},
				wantSub: []string{"1 base", "analytics-base"},
			},
			{
				name:     "branchのみ使用中",
				branches: []string{"feat-a", "feat-b"},
				wantSub:  []string{"2 branches", "feat-a", "feat-b"},
			},
			{
				name:     "baseとbranch両方使用中",
				bases:    []string{"base-1", "base-2"},
				branches: []string{"br-1"},
				wantSub:  []string{"2 bases", "base-1", "base-2", "1 branch", "br-1"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := setupInstalled(t)
				h.usage.bases = tc.bases
				h.usage.branches = tc.branches

				_, err := h.svc.Remove(context.Background(), "clickhouse", false)
				if !errors.Is(err, domain.ErrInUse) {
					t.Fatalf("want ErrInUse, got %v", err)
				}
				for _, sub := range tc.wantSub {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing %q", err.Error(), sub)
					}
				}
				if h.store.removeCalls != 0 || h.restarter.calls != 0 {
					t.Errorf("must not remove or restart when in use")
				}
			})
		}
	})

	t.Run("UsageCheckerがエラーを返した場合は安全側に倒して削除しない", func(t *testing.T) {
		h := setupInstalled(t)
		h.usage.err = errors.New("agent socket unreachable in cluster mode")

		_, err := h.svc.Remove(context.Background(), "clickhouse", false)
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("want usage error, got %v", err)
		}
		if h.store.removeCalls != 0 {
			t.Errorf("must not remove on usage check failure")
		}
	})

	t.Run("未導入エンジンはErrNotFoundで拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		_, err := h.svc.Remove(context.Background(), "clickhouse", false)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("未使用ならモジュールを削除して再起動しOSユーザーと信頼鍵の保持情報を返す", func(t *testing.T) {
		h := setupInstalled(t)
		res, err := h.svc.Remove(context.Background(), "clickhouse", false)
		if err != nil {
			t.Fatalf("Remove failed: %v", err)
		}
		if !res.Removed || !res.Restarted || res.RestartFailed {
			t.Errorf("unexpected flags: %+v", res)
		}
		if res.Engine != "clickhouse" || res.UserKept != "db-branch-rt-clickhouse" || res.KeyIDKept != "acme-2026" {
			t.Errorf("unexpected kept info: %+v", res)
		}
		if h.store.removeCalls != 1 || h.store.removedEngine != "clickhouse" || h.restarter.calls != 1 {
			t.Errorf("expected remove and restart calls")
		}
		if h.keys.remCalls != 0 {
			t.Errorf("Remove must NOT delete trusted key")
		}
	})

	t.Run("noRestart指定時は再起動しない", func(t *testing.T) {
		h := setupInstalled(t)
		res, err := h.svc.Remove(context.Background(), "clickhouse", true)
		if err != nil {
			t.Fatalf("Remove failed: %v", err)
		}
		if !res.Removed || res.Restarted || res.RestartFailed || h.restarter.calls != 0 {
			t.Errorf("expected no restart: %+v calls=%d", res, h.restarter.calls)
		}
	})

	t.Run("再起動に失敗しても削除済み結果とエラーを返す", func(t *testing.T) {
		h := setupInstalled(t)
		h.restarter.err = errors.New("restart failed")

		res, err := h.svc.Remove(context.Background(), "clickhouse", false)
		if err == nil || !strings.Contains(err.Error(), "restart failed") {
			t.Fatalf("expected restart error, got %v", err)
		}
		if !res.Removed || !res.RestartFailed || res.Restarted {
			t.Errorf("expected Removed=true RestartFailed=true, got %+v", res)
		}
	})

	t.Run("破損した導入済みモジュールでも未使用なら削除できる", func(t *testing.T) {
		h := newTestHarness(t)
		h.store.getErr = rtmodule.ErrBadSignature
		h.store.sources["clickhouse"] = rtmodule.ModuleSource{
			Repo:  "github.com/acme/rt-ch",
			Tag:   "v0.2.0",
			KeyID: "acme-2026",
		}

		res, err := h.svc.Remove(context.Background(), "clickhouse", true)
		if err != nil {
			t.Fatalf("expected broken module removal to succeed: %v", err)
		}
		if !res.Removed || res.UserKept != "db-branch-rt-clickhouse" || res.KeyIDKept != "acme-2026" {
			t.Errorf("unexpected result: %+v", res)
		}
	})
}

// Test 31
func TestTrust_鍵の追加と一覧ができ使用中モジュールがある鍵の削除は拒否する(t *testing.T) {
	t.Run("TrustAddは指紋一致時や期待指紋省略時に登録し不一致と重複と不正入力を拒否する", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		other := newTestKeyPair(t, "other-2026")

		// Invalid key_id
		if _, err := h.svc.TrustAdd("../bad", kp.raw, domain.Fingerprint{}); !errors.Is(err, domain.ErrInvalidRef) {
			t.Fatalf("want ErrInvalidRef for bad key_id, got %v", err)
		}
		// Bad public key bytes
		if _, err := h.svc.TrustAdd("acme-2026", []byte("not-base64!!!"), domain.Fingerprint{}); err == nil {
			t.Fatal("expected error for malformed public key")
		}
		// Fingerprint mismatch
		if _, err := h.svc.TrustAdd("acme-2026", kp.raw, other.fp); !errors.Is(err, domain.ErrKeyMismatch) {
			t.Fatalf("want ErrKeyMismatch, got %v", err)
		}
		if h.keys.addCalls != 0 {
			t.Errorf("must not add key on fingerprint mismatch")
		}
		// Fingerprint match succeeds
		tk, err := h.svc.TrustAdd("acme-2026", kp.raw, kp.fp)
		if err != nil {
			t.Fatalf("TrustAdd failed: %v", err)
		}
		if tk.KeyID != "acme-2026" || !tk.Fingerprint.Equal(kp.fp) {
			t.Errorf("unexpected added key: %+v", tk)
		}
		// Duplicate key_id refused with ErrKeyExists
		if _, err := h.svc.TrustAdd("acme-2026", kp.raw, domain.Fingerprint{}); !errors.Is(err, domain.ErrKeyExists) {
			t.Fatalf("want ErrKeyExists, got %v", err)
		}
		// Without expected fingerprint succeeds for a new key_id
		tk2, err := h.svc.TrustAdd("other-2026", other.raw, domain.Fingerprint{})
		if err != nil {
			t.Fatalf("TrustAdd without fingerprint failed: %v", err)
		}
		if tk2.KeyID != "other-2026" {
			t.Errorf("unexpected second key: %+v", tk2)
		}
	})

	t.Run("TrustListは鍵ストアの一覧を返す", func(t *testing.T) {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		_, _ = h.keys.Add(kp.keyID, kp.pub)

		list, err := h.svc.TrustList()
		if err != nil {
			t.Fatalf("TrustList failed: %v", err)
		}
		if len(list) != 1 || list[0].KeyID != "acme-2026" {
			t.Errorf("unexpected list: %+v", list)
		}
	})

	t.Run("TrustRemoveは導入済みモジュールが参照する鍵をErrInUseで拒否し未使用鍵は削除する", func(t *testing.T) {
		h := newTestHarness(t)
		kpUsed := newTestKeyPair(t, "acme-2026")
		kpFree := newTestKeyPair(t, "unused-2026")
		_, _ = h.keys.Add(kpUsed.keyID, kpUsed.pub)
		_, _ = h.keys.Add(kpFree.keyID, kpFree.pub)

		m, _ := makeManifest("clickhouse", "0.2.0", "db-branch-rt-clickhouse")
		h.store.modules["clickhouse"] = rtmodule.VerifiedModule{
			Engine:   "clickhouse",
			Manifest: m,
			KeyID:    "acme-2026",
			Signed:   true,
		}

		// Invalid key_id
		if err := h.svc.TrustRemove(context.Background(), "../bad"); !errors.Is(err, domain.ErrInvalidRef) {
			t.Fatalf("want ErrInvalidRef, got %v", err)
		}
		// Key in use by installed module -> ErrInUse with guidance to remove module first
		err := h.svc.TrustRemove(context.Background(), "acme-2026")
		if !errors.Is(err, domain.ErrInUse) {
			t.Fatalf("want ErrInUse, got %v", err)
		}
		if !strings.Contains(err.Error(), "clickhouse") || !strings.Contains(err.Error(), "module remove") {
			t.Errorf("error %q should mention engine name and module remove guidance", err.Error())
		}
		if _, gErr := h.keys.Get("acme-2026"); gErr != nil {
			t.Errorf("key in use must not be removed")
		}

		// Unused key -> removed successfully
		if err := h.svc.TrustRemove(context.Background(), "unused-2026"); err != nil {
			t.Fatalf("TrustRemove unused key failed: %v", err)
		}
		if _, gErr := h.keys.Get("unused-2026"); !errors.Is(gErr, domain.ErrNotFound) {
			t.Errorf("expected unused key to be deleted")
		}

		// Missing key -> ErrNotFound
		if err := h.svc.TrustRemove(context.Background(), "nonexistent"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestList_導入済みモジュールの正常と未署名と異常状態を一覧で返す(t *testing.T) {
	h := newTestHarness(t)
	mOK, _ := makeManifest("clickhouse", "0.3.0", "db-branch-rt-clickhouse")
	mUnsigned, _ := makeManifest("duckdb", "0.1.0", "db-branch-rt-duckdb")
	h.store.listItems = []rtmodule.VerifiedModule{
		{Engine: "clickhouse", Manifest: mOK, Signed: true, KeyID: "acme-2026"},
		{Manifest: mUnsigned, Signed: false, Warning: "unsigned"},
		{Engine: "broken", Err: rtmodule.ErrBadSignature},
	}

	items := h.svc.List()
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
	if items[0].Engine != "clickhouse" || items[0].Version != "0.3.0" || !items[0].Signed || items[0].KeyID != "acme-2026" || items[0].RunAsUser != "db-branch-rt-clickhouse" || items[0].Status != "ok" || items[0].Err != nil {
		t.Errorf("unexpected item[0]: %+v", items[0])
	}
	if items[1].Engine != "duckdb" || items[1].Signed || items[1].Status != "unsigned" || items[1].Warning != "unsigned" {
		t.Errorf("unexpected item[1]: %+v", items[1])
	}
	if items[2].Engine != "broken" || items[2].Status != "error" || !errors.Is(items[2].Err, rtmodule.ErrBadSignature) {
		t.Errorf("unexpected item[2]: %+v", items[2])
	}
}

type failingTempFile struct {
	name     string
	closeErr error
	buf      bytes.Buffer
}

func (f *failingTempFile) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *failingTempFile) Name() string                { return f.name }
func (f *failingTempFile) Close() error                { return f.closeErr }

func TestInstall_ポートや一時ファイルやデフォルト設定の異常系とフォールバックを正しく処理する(t *testing.T) {
	ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

	setupTrusted := func(t *testing.T) (*testHarness, testKeyPair) {
		t.Helper()
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
			t.Fatal(err)
		}
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
		return h, kp
	}

	t.Run("デフォルトArchとOS一時ファイルとMaxDownloadBytes上書きで動作する", func(t *testing.T) {
		h, kp := setupTrusted(t)
		h.svc.Arch = ""
		h.svc.MaxDownloadBytes = 4096
		h.svc.CreateTemp = nil
		h.svc.RemoveFile = nil

		// Set matching asset for host runtime.GOARCH (amd64 or arm64).
		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if err != nil {
			t.Fatalf("Install with defaults failed: %v", err)
		}
		if !res.Installed || !res.KeyFingerprint.Equal(kp.fp) {
			t.Errorf("unexpected result: %+v", res)
		}
		if h.releases.gotMaxBytes != 4096 {
			t.Errorf("want maxBytes=4096, got %d", h.releases.gotMaxBytes)
		}
		if _, statErr := os.Stat(h.extractor.gotArchive); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("expected default OS temp file %s to be removed", h.extractor.gotArchive)
		}
	})

	t.Run("LockとGetReleaseとExtractとVerifyStagingとUserとWriteSourceの各失敗を伝播する", func(t *testing.T) {
		t.Run("Store.Lock失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.store.lockErr = errors.New("lock busy")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "lock busy") {
				t.Fatalf("want lock error, got %v", err)
			}
		})
		t.Run("GetRelease失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.getErr = errors.New("network down")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "network down") {
				t.Fatalf("want network error, got %v", err)
			}
		})
		t.Run("空タグのRelease", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.releases.release.Tag = ""
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{
				Ref: domain.GitHubRef{Owner: "acme", Repo: "db-branch-rt-clickhouse"},
			}); err == nil {
				t.Fatal("expected error for empty release tag")
			}
		})
		t.Run("CreateTemp失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.svc.CreateTemp = func() (rtmodule.TempFile, error) { return nil, errors.New("no tmp") }
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "no tmp") {
				t.Fatalf("want create temp error, got %v", err)
			}
		})
		t.Run("DownloadAsset失敗時に一時ファイルを削除する", func(t *testing.T) {
			h, _ := setupTrusted(t)
			var removed []string
			h.svc.CreateTemp = func() (rtmodule.TempFile, error) { return &failingTempFile{name: "/fake/dl.tar.gz"}, nil }
			h.svc.RemoveFile = func(p string) error { removed = append(removed, p); return nil }
			h.releases.downloadErr = errors.New("download aborted")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "download aborted") {
				t.Fatalf("want download error, got %v", err)
			}
			if len(removed) != 1 || removed[0] != "/fake/dl.tar.gz" {
				t.Errorf("expected temp file removal, got %v", removed)
			}
		})
		t.Run("一時ファイルClose失敗時に削除する", func(t *testing.T) {
			h, _ := setupTrusted(t)
			var removed []string
			h.svc.CreateTemp = func() (rtmodule.TempFile, error) {
				return &failingTempFile{name: "/fake/dl2.tar.gz", closeErr: errors.New("close failed")}, nil
			}
			h.svc.RemoveFile = func(p string) error { removed = append(removed, p); return nil }
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "close failed") {
				t.Fatalf("want close error, got %v", err)
			}
			if len(removed) != 1 || removed[0] != "/fake/dl2.tar.gz" {
				t.Errorf("expected temp file removal on close error, got %v", removed)
			}
		})
		t.Run("ExtractToStaging失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.extractor.err = domain.ErrUnsafeArchive
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, domain.ErrUnsafeArchive) {
				t.Fatalf("want ErrUnsafeArchive, got %v", err)
			}
		})
		t.Run("VerifyStaging失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.verifier.verifyErr = errors.New("digest mismatch")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("want verify staging error, got %v", err)
			}
			if !h.extractor.cleanedUp {
				t.Errorf("expected cleanup")
			}
		})
		t.Run("VerifyStagingが未署名または別key_idを返したらErrBadSignatureで拒否する", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.verifier.verified.Signed = false
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, rtmodule.ErrBadSignature) {
				t.Fatalf("want ErrBadSignature for unsigned staging, got %v", err)
			}

			h2, _ := setupTrusted(t)
			h2.verifier.verified.KeyID = "different-key"
			if _, err := h2.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, rtmodule.ErrBadSignature) {
				t.Fatalf("want ErrBadSignature for key_id mismatch, got %v", err)
			}
		})
		t.Run("Store.GetがErrNotFound以外のエラーを返したら失敗する", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.store.getErr = errors.New("untrusted existing dir")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "untrusted existing dir") {
				t.Fatalf("want store get error, got %v", err)
			}
		})
		t.Run("EnsureSystemUser失敗", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.users.err = errors.New("useradd failed")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "useradd failed") {
				t.Fatalf("want user error, got %v", err)
			}
			if h.store.commitCalls != 0 || !h.extractor.cleanedUp {
				t.Errorf("must not commit and must cleanup")
			}
		})
		t.Run("WriteSource失敗とnilコールバック", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.extractor.writeSrcErr = errors.New("write source failed")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); err == nil || !strings.Contains(err.Error(), "write source failed") {
				t.Fatalf("want write source error, got %v", err)
			}
			if h.store.commitCalls != 0 || !h.extractor.cleanedUp {
				t.Errorf("must not commit and must cleanup")
			}
		})
	})

	t.Run("署名欠落やパース失敗やインフラ独自エラーをErrBadSignatureでラップする", func(t *testing.T) {
		t.Run("署名バイト列が空", func(t *testing.T) {
			h, _ := setupTrusted(t)
			h.extractor.mod.SignatureRaw = nil
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, rtmodule.ErrBadSignature) {
				t.Fatalf("want ErrBadSignature, got %v", err)
			}
		})
		t.Run("ParseSignatureが独自エラーを返してもErrBadSignatureとしてラップされる", func(t *testing.T) {
			h, _ := setupTrusted(t)
			infraErr := errors.New("infra bad sig")
			h.verifier.parseSigErr = infraErr
			_, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
			if !errors.Is(err, rtmodule.ErrBadSignature) || !errors.Is(err, infraErr) {
				t.Fatalf("want wrapped ErrBadSignature+infraErr, got %v", err)
			}
		})
		t.Run("候補鍵のParsePublicKey失敗はErrBadSignatureになる", func(t *testing.T) {
			h := newTestHarness(t)
			kp := newTestKeyPair(t, "acme-2026")
			h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, true)
			h.extractor.mod.CandidateKeyRaw = []byte("corrupt-pub")
			if _, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref}); !errors.Is(err, rtmodule.ErrBadSignature) {
				t.Fatalf("want ErrBadSignature, got %v", err)
			}
		})
		t.Run("登録済み鍵のFingerprintが空でもPublicKeyから補完される", func(t *testing.T) {
			h, kp := setupTrusted(t)
			tk := h.keys.keys[kp.keyID]
			tk.Fingerprint = domain.Fingerprint{}
			h.keys.keys[kp.keyID] = tk
			res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
			if err != nil {
				t.Fatalf("Install failed: %v", err)
			}
			if !res.KeyFingerprint.Equal(kp.fp) {
				t.Errorf("expected computed fingerprint %s, got %s", kp.fp, res.KeyFingerprint)
			}
		})
	})

	t.Run("RemoveとTrustRemoveのロック失敗や削除失敗やEngine補完を処理する", func(t *testing.T) {
		h := newTestHarness(t)
		h.store.lockErr = errors.New("lock err")
		if _, err := h.svc.Remove(context.Background(), "clickhouse", true); err == nil || !strings.Contains(err.Error(), "lock err") {
			t.Fatalf("Remove: want lock error, got %v", err)
		}
		if err := h.svc.TrustRemove(context.Background(), "acme-2026"); err == nil || !strings.Contains(err.Error(), "lock err") {
			t.Fatalf("TrustRemove: want lock error, got %v", err)
		}

		h2 := newTestHarness(t)
		m, _ := makeManifest("clickhouse", "0.2.0", "db-branch-rt-clickhouse")
		h2.store.modules["clickhouse"] = rtmodule.VerifiedModule{Manifest: m, KeyID: "acme-2026", Signed: true}
		h2.store.removeErr = errors.New("remove failed")
		if _, err := h2.svc.Remove(context.Background(), "clickhouse", true); err == nil || !strings.Contains(err.Error(), "remove failed") {
			t.Fatalf("Remove: want remove error, got %v", err)
		}
		// TrustRemove uses Manifest.Engine when VerifiedModule.Engine is empty
		if err := h2.svc.TrustRemove(context.Background(), "acme-2026"); !errors.Is(err, domain.ErrInUse) || !strings.Contains(err.Error(), "clickhouse") {
			t.Fatalf("TrustRemove: want ErrInUse mentioning clickhouse, got %v", err)
		}
	})
}

func TestInstall_クラスター構成のノードでは単一ノード限定の実験的機能である警告を返す(t *testing.T) {
	for _, clustered := range []bool{true, false} {
		h := newTestHarness(t)
		kp := newTestKeyPair(t, "acme-2026")
		if _, err := h.keys.Add(kp.keyID, kp.pub); err != nil {
			t.Fatal(err)
		}
		h.setupValidRelease(t, "clickhouse", "0.2.0", "v0.2.0", kp, false)
		h.svc.Clustered = func() bool { return clustered }
		ref, _ := domain.ParseGitHubRef("github.com/acme/db-branch-rt-clickhouse@v0.2.0")

		res, err := h.svc.Install(context.Background(), rtmodule.InstallRequest{Ref: ref})
		if err != nil {
			t.Fatalf("Install failed: %v", err)
		}
		found := false
		for _, w := range res.Warnings {
			if strings.Contains(w, rtmodule.ClusterWarning) {
				found = true
			}
		}
		if found != clustered {
			t.Errorf("clustered=%v: warning present=%v, warnings=%v", clustered, found, res.Warnings)
		}
	}
}

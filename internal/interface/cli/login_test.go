package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

type memSecretBackend struct {
	available bool
	tokens    map[string]string
	failSave  bool
}

func (m *memSecretBackend) Name() string    { return "mem-keychain" }
func (m *memSecretBackend) Available() bool { return m.available }
func (m *memSecretBackend) Save(profile, token string) error {
	if m.failSave {
		return errors.New("keychain locked")
	}
	if m.tokens == nil {
		m.tokens = map[string]string{}
	}
	m.tokens[profile] = token
	return nil
}
func (m *memSecretBackend) Load(profile string) (string, error) {
	tok, ok := m.tokens[profile]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return tok, nil
}
func (m *memSecretBackend) Delete(profile string) error {
	delete(m.tokens, profile)
	return nil
}

type fakeRemoteRunner struct {
	api         *fakeAPI
	lastOpts    cli.RemoteOptions
	probeResult cli.CertSummary
	probeErr    error
	onProbe     func(url, ca, sha string) (cli.CertSummary, error)
}

func runRemoteCLI(t *testing.T, cfgPath string, osKC *memSecretBackend, env map[string]string, stdin string, isTTY bool, r *fakeRemoteRunner, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := cli.App{
		Version:    "1.0.0",
		ClientOnly: true,
		Stdin:      strings.NewReader(stdin),
		Stdout:     &out,
		Stderr:     &errOut,
		IsTTY:      func(io.Reader) bool { return isTTY },
		Getenv: func(k string) string {
			return env[k]
		},
		NewClientStores: func(path string) (cli.ClientConfigStore, cli.SecretStore, error) {
			if path == "" {
				path = cfgPath
			}
			st := keychain.NewWithBackends(osKC, keychain.NewFileBackend(clientcfg.CredentialsPath(path)))
			return cli.FileClientConfigStore(path), st, nil
		},
		ProbeTLS: func(_ context.Context, rawURL, ca, sha string) (cli.CertSummary, error) {
			if r.onProbe != nil {
				return r.onProbe(rawURL, ca, sha)
			}
			return r.probeResult, r.probeErr
		},
		RemoteClient: func(opts cli.RemoteOptions) (cli.Client, error) {
			r.lastOpts = opts
			return r.api, nil
		},
	}
	code := app.Run(context.Background(), args)
	return code, out.String(), errOut.String()
}

func TestLogin_TOFU対話承認で保存しその後のコマンドで使える(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "client.toml")
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "node1"}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{}}

	r := &fakeRemoteRunner{
		api: api,
		onProbe: func(_, _, sha string) (cli.CertSummary, error) {
			cert := cli.CertSummary{
				Subject:   "CN=db-branch",
				Issuer:    "CN=db-branch",
				NotBefore: time.Now().Add(-time.Hour),
				NotAfter:  time.Now().Add(24 * time.Hour),
				SANs:      []string{"10.0.0.95"},
				SHA256:    strings.Repeat("ab", 32),
			}
			if sha == "" {
				return cert, &cli.UntrustedCertError{Cert: cert, Err: errors.New("unknown authority")}
			}
			return cert, nil
		},
	}

	// Non-TTY without --trust-sha256 must refuse untrusted cert.
	code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_1_sec\n", false, r,
		"login", "--server", "https://10.0.0.95:7080", "--token-stdin")
	if code == 0 || !strings.Contains(errOut, "--trust-sha256") {
		t.Fatalf("non-TTY TOFU should fail with guidance, code=%d err=%s", code, errOut)
	}

	// TTY confirms 'y' with token prompt -> saves profile and token in keychain.
	code, out, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_1_sec\ny\n", true, r,
		"login", "--server", "https://10.0.0.95:7080", "--profile", "prod")
	if code != 0 {
		t.Fatalf("TTY login failed (%d): %s", code, errOut)
	}
	if !strings.Contains(out, "prod") || osKC.tokens["prod"] != "dbb_1_sec" {
		t.Errorf("unexpected login result: out=%q tokens=%v", out, osKC.tokens)
	}

	// Subsequent command uses saved profile and pinned SHA256.
	code, out, errOut = runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "branch", "list")
	if code != 0 {
		t.Fatalf("branch list failed (%d): %s", code, errOut)
	}
	if r.lastOpts.URL != "https://10.0.0.95:7080" || r.lastOpts.Token != "dbb_1_sec" || r.lastOpts.PinnedSHA256 != strings.Repeat("ab", 32) {
		t.Errorf("lastOpts = %+v", r.lastOpts)
	}
}

func TestLogin_キーチェーン不可なら0600ファイルに保存し警告を出す(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "client.toml")
	osKC := &memSecretBackend{available: false}
	api := newFakeAPI()
	api.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "node1"}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	r := &fakeRemoteRunner{api: api}

	code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_2_sec\n", false, r,
		"login", "--server", "https://dbb.example.com", "--token-stdin")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "0600") {
		t.Errorf("expected 0600 warning, got %q", errOut)
	}
	credInfo, err := os.Stat(clientcfg.CredentialsPath(cfgPath))
	if err != nil || credInfo.Mode().Perm() != 0o600 {
		t.Errorf("cred file stat = (%v, %v)", credInfo, err)
	}
}

func TestServerOverride_プロファイル未指定のServerは現在プロファイルのトークンを流用しない(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "client.toml")
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "node1"}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	r := &fakeRemoteRunner{api: api}

	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_prod_tok\n", false, r,
		"login", "--server", "https://prod.example.com", "--token-stdin"); code != 0 {
		t.Fatal(errOut)
	}

	// Calling --server without --profile and without DB_BRANCH_TOKEN must NOT send prod's token.
	code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r,
		"--server", "https://evil.example.com", "branch", "list")
	if code == 0 || !strings.Contains(errOut, "DB_BRANCH_TOKEN") {
		t.Fatalf("expected error preventing token leak, code=%d err=%s", code, errOut)
	}

	// With DB_BRANCH_URL + DB_BRANCH_TOKEN, works with no saved config at all.
	code, _, errOut = runRemoteCLI(t, filepath.Join(t.TempDir(), "none.toml"), osKC, map[string]string{
		"DB_BRANCH_URL":   "https://ci.example.com",
		"DB_BRANCH_TOKEN": "dbb_ci_tok",
	}, "", false, r, "branch", "list")
	if code != 0 {
		t.Fatalf("CI env failed (%d): %s", code, errOut)
	}
	if r.lastOpts.URL != "https://ci.example.com" || r.lastOpts.Token != "dbb_ci_tok" || r.lastOpts.PinnedSHA256 != "" {
		t.Errorf("unexpected CI opts: %+v", r.lastOpts)
	}
}

func TestProfileAndLogout_一覧と切替と削除ができる(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "client.toml")
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "node1"}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	r := &fakeRemoteRunner{api: api}

	for _, p := range []struct{ name, url, tok string }{
		{"dev", "https://dev.example.com", "dbb_dev"},
		{"prod", "https://prod.example.com", "dbb_prod"},
	} {
		if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, p.tok+"\n", false, r,
			"login", "--profile", p.name, "--server", p.url, "--token-stdin"); code != 0 {
			t.Fatal(errOut)
		}
	}

	code, out, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "list")
	if code != 0 || !strings.Contains(out, "* prod") || !strings.Contains(out, "dev") {
		t.Fatalf("profile list = (%d, %q, %q)", code, out, errOut)
	}

	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "use", "dev"); code != 0 {
		t.Fatalf("profile use = %s", errOut)
	}
	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "logout"); code != 0 {
		t.Fatalf("logout = %s", errOut)
	}
	if _, ok := osKC.tokens["dev"]; ok {
		t.Error("dev token should be deleted after logout")
	}
	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "remove", "prod"); code != 0 {
		t.Fatalf("profile remove = %s", errOut)
	}
	if _, ok := osKC.tokens["prod"]; ok {
		t.Error("prod token should be deleted after profile remove")
	}
}

func TestLogin_追加フラグとエラー分岐とJSON出力を網羅する(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "client.toml")
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "node1"}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	r := &fakeRemoteRunner{api: api}

	// Empty profile list (table + json)
	if code, out, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "list"); code != 0 || !strings.Contains(out, "No profiles") {
		t.Fatalf("empty profile list = (%d, %q)", code, out)
	}

	// --token flag warns about ps and --no-use preserves empty current_profile, -o json outputs json.
	code, out, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r,
		"login", "--server", "https://one.example.com", "--profile", "one", "--token", "dbb_one", "--no-use", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "Warning:") || !strings.Contains(out, `"profile": "one"`) {
		t.Fatalf("login --token --no-use = (%d, %q, %q)", code, out, errOut)
	}

	// --ca flag & interactive URL prompt in TTY mode.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(caFile, []byte("PEM"), 0o600)
	code, _, errOut = runRemoteCLI(t, cfgPath, osKC, nil, "https://two.example.com\ndbb_two\n", true, r,
		"login", "--profile", "two", "--ca", caFile)
	if code != 0 {
		t.Fatalf("interactive login --ca failed (%d): %s", code, errOut)
	}

	// Profile list JSON includes both profiles with tls modes, plus pinned profile in table mode.
	code, out, _ = runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "list", "-o", "json")
	if code != 0 || !strings.Contains(out, `"tls": "ca"`) {
		t.Fatalf("profile list json = (%d, %q)", code, out)
	}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "profile", "list", "-o", "table"); code != 0 {
		t.Fatal("profile list table failed")
	}

	// Cert rotation on existing pinned profile warns and rejects non-TTY, accepts TTY 'y'.
	oldFP := strings.Repeat("aa", 32)
	newFP := strings.Repeat("bb", 32)
	rPin := &fakeRemoteRunner{
		api: api,
		onProbe: func(_, _, sha string) (cli.CertSummary, error) {
			cert := cli.CertSummary{
				Subject: "CN=rot", Issuer: "CN=rot",
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
				SANs: []string{"rot.example.com"}, SHA256: oldFP, SANMismatch: true,
			}
			if sha == "" {
				return cert, &cli.UntrustedCertError{Cert: cert, Err: errors.New("untrusted")}
			}
			return cert, nil
		},
	}
	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_rot\n", false, rPin,
		"login", "--profile", "rot", "--server", "https://rot.example.com", "--token-stdin", "--trust-sha256", oldFP); code != 0 {
		t.Fatal(errOut)
	}
	// Profile list shows "pinned" mode for rot.
	if code, out, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, rPin, "profile", "list"); code != 0 || !strings.Contains(out, "pinned") {
		t.Fatalf("expected pinned in profile list, got (%d, %q)", code, out)
	}
	// Re-login with same cert via DB_BRANCH_PROFILE reuses existing URL without --server.
	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, map[string]string{"DB_BRANCH_PROFILE": "rot"}, "dbb_rot\n", false, rPin,
		"login", "--token-stdin"); code != 0 {
		t.Fatalf("re-login same cert via DB_BRANCH_PROFILE failed: %s", errOut)
	}
	// Rotate cert -> non-TTY fails, TTY 'n' fails, TTY 'y' updates pin.
	rPin.onProbe = func(_, _, sha string) (cli.CertSummary, error) {
		cert := cli.CertSummary{Subject: "CN=rot", Issuer: "CN=rot", NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), SHA256: newFP}
		if sha == "" {
			return cert, &cli.UntrustedCertError{Cert: cert, Err: errors.New("untrusted")}
		}
		return cert, nil
	}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_rot\n", false, rPin,
		"login", "--profile", "rot", "--token-stdin"); code == 0 {
		t.Error("rotated cert non-TTY should fail")
	}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_rot\nn\n", true, rPin,
		"login", "--profile", "rot", "--token-stdin"); code == 0 {
		t.Error("declining rotated cert should fail")
	}
	if code, _, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "dbb_rot\ny\n", true, rPin,
		"login", "--profile", "rot", "--token-stdin"); code != 0 || !strings.Contains(errOut, "changed") {
		t.Fatalf("TTY accept rotated cert failed (%d): %s", code, errOut)
	}

	// logout --all clears remaining tokens.
	if code, out, errOut := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, "logout", "--all"); code != 0 || !strings.Contains(out, "one") {
		t.Fatalf("logout --all = (%d, %q, %q)", code, out, errOut)
	}

	// Invalid inputs coverage.
	badCases := [][]string{
		{"login", "--server", "http://insecure.example.com", "--token", "x"},
		{"login", "--server", "https://x.example.com", "--token", "x", "--token-stdin"},
		{"login", "--server", "https://x.example.com"},
		{"login", "--server", "https://x.example.com", "--token", "   "},
		{"login", "--server", "https://x.example.com", "--token", "x", "--trust-sha256", "bad"},
		{"login", "--profile", "bad/name", "--server", "https://x.example.com", "--token", "x"},
		{"profile", "use", "missing"},
		{"profile", "remove", "missing"},
		{"--profile", "missing", "branch", "list"},
		{"--profile", "bad/name", "branch", "list"},
	}
	for _, args := range badCases {
		if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r, args...); code == 0 {
			t.Errorf("expected non-zero exit for %v", args)
		}
	}
	// TTY EOF on server prompt fails cleanly.
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", true, r, "login"); code == 0 {
		t.Error("TTY EOF on server prompt should fail")
	}

	// Non-empty capabilities version check.
	api.answers["GET /v1/capabilities"] = map[string]any{"version": ""}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, r,
		"login", "--server", "https://one.example.com", "--token", "x"); code == 0 {
		t.Error("empty capabilities version should fail")
	}
}

type stubSrcErr struct{}

func (stubSrcErr) Error() string                   { return "raw" }
func (stubSrcErr) APIErrorParts() (int, string, string) { return 403, "forbidden", "nope" }

func TestRemoteAdapterAndEdgeCases_APIError変換や未設定フックを扱う(t *testing.T) {
	// UntrustedCertError methods & FileClientConfigStore.Path
	uce := &cli.UntrustedCertError{Cert: cli.CertSummary{SHA256: "abc"}, Err: errors.New("bad")}
	if !strings.Contains(uce.Error(), "abc") || uce.Unwrap() == nil {
		t.Errorf("UntrustedCertError = %v", uce)
	}
	st := cli.FileClientConfigStore("/tmp/c.toml")
	if st.Path() != "/tmp/c.toml" {
		t.Errorf("Path = %q", st.Path())
	}

	// APIError adaptation via RemoteClient wrapper
	var out, errOut bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.errors["GET /v1/branches"] = stubSrcErr{}
	app := cli.App{
		ClientOnly: true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv:     func(k string) string { return map[string]string{"DB_BRANCH_URL": "https://x", "DB_BRANCH_TOKEN": "t"}[k] },
		NewClientStores: func(string) (cli.ClientConfigStore, cli.SecretStore, error) {
			return cli.FileClientConfigStore(cfgPath), keychain.NewWithBackends(osKC, keychain.NewFileBackend(clientcfg.CredentialsPath(cfgPath))), nil
		},
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api, nil },
	}
	code := app.Run(context.Background(), []string{"branch", "list"})
	if code == 0 || !strings.Contains(errOut.String(), "forbidden") {
		t.Errorf("APIError adapt = (%d, %q)", code, errOut.String())
	}

	// Unconfigured hooks / missing current profile / missing token / pipe termReadSecret
	unconf := cli.App{ClientOnly: true, Stdout: &out, Stderr: &errOut}
	if code := unconf.Run(context.Background(), []string{"profile", "list"}); code == 0 {
		t.Error("missing NewClientStores should fail")
	}
	if code := unconf.Run(context.Background(), []string{"--server", "https://x", "branch", "list"}); code == 0 {
		t.Error("missing token should fail")
	}
	unconf.Getenv = func(k string) string { return map[string]string{"DB_BRANCH_URL": "https://x", "DB_BRANCH_TOKEN": "t"}[k] }
	if code := unconf.Run(context.Background(), []string{"branch", "list"}); code == 0 {
		t.Error("missing RemoteClient should fail")
	}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, &fakeRemoteRunner{api: api}, "logout"); code == 0 {
		t.Error("logout without active profile should fail")
	}
	if code, _, _ := runRemoteCLI(t, cfgPath, osKC, nil, "", false, &fakeRemoteRunner{api: api}, "branch", "list"); code == 0 {
		t.Error("branch list without active profile should fail")
	}

	// ReadSecret with nil ProbeTLS and os.Pipe fallback in termReadSecret
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pw.WriteString("dbb_pipe_tok\n")
	_ = pw.Close()
	appPipe := cli.App{
		ClientOnly: true,
		Stdin:      pr,
		Stdout:     &out,
		Stderr:     &errOut,
		IsTTY:      func(io.Reader) bool { return true },
		NewClientStores: func(string) (cli.ClientConfigStore, cli.SecretStore, error) {
			return cli.FileClientConfigStore(cfgPath), keychain.NewWithBackends(osKC, keychain.NewFileBackend(clientcfg.CredentialsPath(cfgPath))), nil
		},
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) {
			okAPI := newFakeAPI()
			okAPI.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "n1"}
			okAPI.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
			return okAPI, nil
		},
	}
	if code := appPipe.Run(context.Background(), []string{"login", "--server", "https://pipe.example.com"}); code != 0 {
		t.Errorf("pipe login failed (%d): %s", code, errOut.String())
	}

	// Missing token in secret store for existing profile
	_ = osKC.Delete("default")
	if code := appPipe.Run(context.Background(), []string{"branch", "list"}); code == 0 {
		t.Error("missing token in secret store should fail")
	}

	// Custom ReadSecret hook, system TLS mode in profile list, RemoteClient factory error, non-untrusted ProbeTLS error.
	osKC2 := &memSecretBackend{available: true}
	cfgPath2 := filepath.Join(t.TempDir(), "c2.toml")
	api2 := newFakeAPI()
	api2.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "n1"}
	api2.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	appCustom := cli.App{
		ClientOnly: true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		IsTTY:      func(io.Reader) bool { return true },
		ReadSecret: func(io.Reader) (string, error) { return "dbb_hook", nil },
		NewClientStores: func(string) (cli.ClientConfigStore, cli.SecretStore, error) {
			return cli.FileClientConfigStore(cfgPath2), keychain.NewWithBackends(osKC2, keychain.NewFileBackend(clientcfg.CredentialsPath(cfgPath2))), nil
		},
		ProbeTLS:     func(context.Context, string, string, string) (cli.CertSummary, error) { return cli.CertSummary{}, nil },
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api2, nil },
	}
	if code := appCustom.Run(context.Background(), []string{"login", "--server", "https://sys.example.com"}); code != 0 {
		t.Fatalf("custom ReadSecret login failed (%d): %s", code, errOut.String())
	}
	// If /v1/capabilities returns 200 but /v1/branches returns 401, login MUST fail.
	apiBadBranches := newFakeAPI()
	apiBadBranches.answers["GET /v1/capabilities"] = map[string]any{"version": "1.0.0", "node": "n1"}
	apiBadBranches.errors["GET /v1/branches"] = &cli.APIError{Status: 401, Code: "unauthenticated", Message: "invalid token"}
	appCustom.RemoteClient = func(cli.RemoteOptions) (cli.Client, error) { return apiBadBranches, nil }
	if code := appCustom.Run(context.Background(), []string{"login", "--server", "https://sys.example.com", "--token", "bad"}); code == 0 {
		t.Error("login must fail when GET /v1/branches rejects the token")
	}
	appCustom.RemoteClient = func(cli.RemoteOptions) (cli.Client, error) { return api2, nil }

	// --ca + --trust-sha256 together must fail; DB_BRANCH_TLS_SHA256 overrides saved ca_cert.
	caFile2 := filepath.Join(t.TempDir(), "ca2.pem")
	_ = os.WriteFile(caFile2, []byte("PEM"), 0o600)
	if code := appCustom.Run(context.Background(), []string{"login", "--server", "https://sys.example.com", "--token", "x", "--ca", caFile2, "--trust-sha256", strings.Repeat("aa", 32)}); code == 0 {
		t.Error("--ca and --trust-sha256 together should fail")
	}
	if code, _, errOut := runRemoteCLI(t, cfgPath2, osKC2, nil, "tok\n", false, &fakeRemoteRunner{api: api2},
		"login", "--profile", "ca-prof", "--server", "https://ca.example.com", "--ca", caFile2, "--token-stdin"); code != 0 {
		t.Fatal(errOut)
	}
	rOverride := &fakeRemoteRunner{api: api2}
	pinOverride := strings.Repeat("cc", 32)
	if code, _, errOut := runRemoteCLI(t, cfgPath2, osKC2, map[string]string{"DB_BRANCH_TLS_SHA256": pinOverride}, "", false, rOverride,
		"--profile", "ca-prof", "branch", "list"); code != 0 {
		t.Fatalf("DB_BRANCH_TLS_SHA256 override failed: %s", errOut)
	}
	if rOverride.lastOpts.PinnedSHA256 != pinOverride || rOverride.lastOpts.CACert != "" {
		t.Errorf("expected DB_BRANCH_TLS_SHA256 to override ca_cert, got %+v", rOverride.lastOpts)
	}
	if code, _, _ := runRemoteCLI(t, cfgPath2, osKC2, map[string]string{"DB_BRANCH_TLS_SHA256": pinOverride, "DB_BRANCH_CA": caFile2}, "", false, rOverride,
		"--profile", "ca-prof", "branch", "list"); code == 0 {
		t.Error("DB_BRANCH_CA + DB_BRANCH_TLS_SHA256 conflict should fail")
	}

	// Node CLI (ClientOnly: false) with DB_BRANCH_TOKEN set must still use local socket.
	sockAPI := newFakeAPI()
	sockAPI.answers["GET /v1/branches"] = map[string]any{"items": []any{}}
	nodeApp := cli.App{
		ClientOnly: false,
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv:     func(k string) string { return map[string]string{"DB_BRANCH_TOKEN": "dbb_stray"}[k] },
		Client:     func(string) cli.Client { return sockAPI },
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) {
			t.Fatal("DB_BRANCH_TOKEN alone must not hijack local socket CLI")
			return nil, nil
		},
	}
	if code := nodeApp.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "none.toml"), "branch", "list"}); code != 0 {
		t.Fatalf("node socket CLI with DB_BRANCH_TOKEN failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	if code := appCustom.Run(context.Background(), []string{"profile", "list"}); code != 0 || !strings.Contains(out.String(), "system") {
		t.Fatalf("profile list system mode = (%d, %q)", code, out.String())
	}
	appCustom.ProbeTLS = func(context.Context, string, string, string) (cli.CertSummary, error) {
		return cli.CertSummary{}, errors.New("dial timeout")
	}
	if code := appCustom.Run(context.Background(), []string{"login", "--server", "https://sys.example.com", "--token", "x"}); code == 0 {
		t.Error("non-untrusted ProbeTLS error should fail")
	}
	appCustom.ProbeTLS = nil
	appCustom.RemoteClient = func(cli.RemoteOptions) (cli.Client, error) { return nil, errors.New("bad tls") }
	if code := appCustom.Run(context.Background(), []string{"login", "--server", "https://sys.example.com", "--token", "x"}); code == 0 {
		t.Error("RemoteClient error should fail")
	}

	// Corrupted client.toml surfaces Load error across login, logout, profile, and remoteClient.
	badCfg := filepath.Join(t.TempDir(), "bad.toml")
	_ = os.WriteFile(badCfg, []byte("broken =\n"), 0o600)
	rBad := &fakeRemoteRunner{api: api2}
	for _, args := range [][]string{
		{"--config", badCfg, "login", "--server", "https://x", "--token", "t"},
		{"--config", badCfg, "logout"},
		{"--config", badCfg, "profile", "list"},
		{"--config", badCfg, "profile", "use", "a"},
		{"--config", badCfg, "branch", "list"},
		{"profile", "use", "bad/name"},
	} {
		if code, _, _ := runRemoteCLI(t, cfgPath2, osKC2, nil, "", false, rBad, args...); code == 0 {
			t.Errorf("expected error for %v", args)
		}
	}

	// SecretStore Save error and Delete error.
	osFailSave := &memSecretBackend{available: false}
	dirAsCred := filepath.Join(t.TempDir(), "cred-dir")
	_ = os.Mkdir(dirAsCred, 0o700)
	appSaveErr := cli.App{
		ClientOnly: true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		NewClientStores: func(string) (cli.ClientConfigStore, cli.SecretStore, error) {
			return cli.FileClientConfigStore(filepath.Join(t.TempDir(), "c.toml")), keychain.NewWithBackends(osFailSave, keychain.NewFileBackend(dirAsCred)), nil
		},
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api2, nil },
	}
	if code := appSaveErr.Run(context.Background(), []string{"login", "--server", "https://x.example.com", "--token", "x"}); code == 0 {
		t.Error("SecretStore Save error should fail")
	}
	if code := appSaveErr.Run(context.Background(), []string{"logout", "--profile", "default"}); code == 0 {
		t.Error("SecretStore Delete error should fail")
	}

	// Empty stdin read error in non-TTY / EOF.
	appNilStdin := cli.App{ClientOnly: true, Stdin: nil, Stdout: &out, Stderr: &errOut, IsTTY: func(io.Reader) bool { return true },
		NewClientStores: appCustom.NewClientStores, RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api2, nil }}
	if code := appNilStdin.Run(context.Background(), []string{"login", "--server", "https://x.example.com"}); code == 0 {
		t.Error("nil stdin should fail")
	}
	appEOFStdin := cli.App{ClientOnly: true, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		NewClientStores: appCustom.NewClientStores, RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api2, nil }}
	if code := appEOFStdin.Run(context.Background(), []string{"login", "--server", "https://x.example.com", "--token-stdin"}); code == 0 {
		t.Error("EOF on --token-stdin should fail")
	}
}



package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

var ctx = context.Background()

// The test binary doubles as a module when started with this variable.
func TestMain(m *testing.M) {
	if out := os.Getenv("DBB_TEST_MODULE_OUT"); out != "" {
		plugin.Serve(ext.Module{Name: "test-module", Version: "0.1.0",
			Authenticator: tokens{}, Authorizer: readOnly{}, AuditSink: fileSink(out),
			Router: noRouter{}, Ports: map[string]int{"mysql": 6033},
			Settings: []ext.Setting{{Key: "k", Type: "string"}}, Configurable: noConfig{},
			Handler: hello{}, Pages: []ext.Page{{Path: "/ui/", Title: "Test"}}, Starter: &keeper{}, RequiresLicense: true,
			PublicPaths: []string{"/sso/"}, LoginMethods: []ext.LoginMethod{{Label: "SSO", Path: "/sso/start"}},
			BranchPolicy: twoMax{}})
		return
	}
	os.Exit(m.Run())
}

type tokens struct{}

func (tokens) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	switch c.Token {
	case "":
		return ext.Principal{}, ext.ErrUnauthenticated
	case "good":
		return ext.Principal{ID: "token:alice", Name: "alice", Roles: []string{"viewer"}}, nil
	}
	return ext.Principal{}, errors.New("token revoked")
}

// ChangePassword makes tokens a PasswordChanger: "alice" may change hers.
func (tokens) ChangePassword(_ context.Context, p ext.Principal, current, next string) error {
	if p.ID != "token:alice" || current != "old" {
		return ext.ErrUnauthenticated
	}
	if len(next) < 12 {
		return errors.New("too short")
	}
	return nil
}

// twoMax allows branches from base "prod" only and shares with "team:" owners.
type twoMax struct{}

func (twoMax) CheckBranch(_ context.Context, _ ext.Principal, r ext.BranchRequest) error {
	if r.Base != "prod" || len(r.Existing) >= 2 {
		return ext.ErrForbidden
	}
	return nil
}

func (twoMax) SharesBranches(_ context.Context, _ ext.Principal, owner string) bool {
	return strings.HasPrefix(owner, "team:")
}

type hello struct{}

func (hello) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	return ext.Response{Status: 200, Body: []byte("hello " + r.Principal.Name)}, nil
}

// keeper writes what it is given into its store, so the test sees Start work.
type keeper struct{}

func (*keeper) Start(ctx context.Context, h ext.Host) error {
	return h.Store.Put(ctx, "started-on", []byte(h.Node))
}

type noConfig struct{}

func (noConfig) Configure(context.Context, map[string]string) error { return nil }

type noRouter struct{}

func (noRouter) Route(context.Context, ext.RouteConfig) error { return nil }

type readOnly struct{}

func (readOnly) Authorize(_ context.Context, p ext.Principal, action string, _ ext.Resource) error {
	if len(p.Roles) > 0 && p.Roles[0] == "viewer" && action != "branch.read" {
		return fmt.Errorf("%w: viewers only read", ext.ErrForbidden)
	}
	return nil
}

type fileSink string

func (f fileSink) Record(_ context.Context, e ext.Event) error {
	if e.Action == "fail" {
		return errors.New("sink failed")
	}
	b, _ := json.Marshal(e)
	fh, err := os.OpenFile(string(f), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.Write(append(b, '\n'))
	return err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// moduleDir holds a launcher that starts this test binary as a module.
func moduleDir(t *testing.T, out string) string {
	dir := t.TempDir()
	self, _ := os.Executable()
	script := "#!/bin/sh\nDBB_TEST_MODULE_OUT=" + out + " exec " + self + "\n"
	if err := os.WriteFile(filepath.Join(dir, "10-test"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("not a module"), 0o644)
	return dir
}

func TestLoad_別プロセスのモジュールを起動し各ポートを呼べる(t *testing.T) {
	out := filepath.Join(t.TempDir(), "audit.jsonl")
	mods, err := plugin.Load(moduleDir(t, out), quiet())
	if err != nil || len(mods) != 1 {
		t.Fatalf("Load = %v %v", mods, err)
	}
	m := mods[0]
	defer m.Close()
	if m.Info.Name != "test-module" || m.Module.Authenticator == nil || m.Module.Authorizer == nil || m.Module.AuditSink == nil ||
		m.Module.Router == nil || m.Module.Ports["mysql"] != 6033 || m.Module.Configurable == nil || len(m.Module.Settings) != 1 || !m.Module.RequiresLicense || len(m.Module.PublicPaths) != 1 || len(m.Module.LoginMethods) != 1 {
		t.Fatalf("info = %+v", m.Info)
	}
	p, err := m.Module.Authenticator.Authenticate(ctx, ext.Credentials{Token: "good"})
	if err != nil || p.ID != "token:alice" || p.Roles[0] != "viewer" {
		t.Errorf("good token: %+v %v", p, err)
	}
	if _, err := m.Module.Authenticator.Authenticate(ctx, ext.Credentials{Username: "admin", Password: "x"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("not a token: %v", err)
	}
	if _, err := m.Module.Authenticator.Authenticate(ctx, ext.Credentials{Token: "bad"}); err == nil || errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
	bp := m.Module.BranchPolicy
	if bp == nil || bp.CheckBranch(ctx, p, ext.BranchRequest{Base: "prod"}) != nil ||
		!errors.Is(bp.CheckBranch(ctx, p, ext.BranchRequest{Base: "other"}), ext.ErrForbidden) ||
		!bp.SharesBranches(ctx, p, "team:x") || bp.SharesBranches(ctx, p, "solo") {
		t.Error("branch policy over RPC")
	}
	pc, ok := m.Module.Authenticator.(ext.PasswordChanger)
	if !ok {
		t.Fatal("the module's users can change passwords")
	}
	if err := pc.ChangePassword(ctx, p, "old", "a long new passphrase"); err != nil {
		t.Errorf("change: %v", err)
	}
	if err := pc.ChangePassword(ctx, p, "wrong", "a long new passphrase"); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("wrong current: %v", err)
	}
	if err := pc.ChangePassword(ctx, p, "old", "short"); err == nil || errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("rejected: %v", err)
	}
	if err := m.Module.Authorizer.Authorize(ctx, p, "branch.delete", ext.Resource{Kind: "branch", Name: "x"}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("viewer delete: %v", err)
	}
	if err := m.Module.Authorizer.Authorize(ctx, p, "branch.read", ext.Resource{Kind: "branch"}); err != nil {
		t.Errorf("viewer read: %v", err)
	}
	if err := m.Module.AuditSink.Record(ctx, ext.Event{Action: "branch.create", Principal: p, Outcome: ext.OutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if err := m.Module.AuditSink.Record(ctx, ext.Event{Action: "fail"}); err == nil {
		t.Error("sink failure: want error")
	}
	if resp, err := m.Module.Handler.Serve(ctx, ext.Request{Path: "/", Principal: p}); err != nil || string(resp.Body) != "hello alice" {
		t.Errorf("serve = %+v %v", resp, err)
	}
	if len(m.Module.Pages) != 1 {
		t.Errorf("pages = %v", m.Module.Pages)
	}
	data := memStore{}
	if err := m.Module.Starter.Start(ctx, ext.Host{Node: "n1", Store: data}); err != nil || string(data["started-on"]) != "n1" {
		t.Errorf("start: %v %v", err, data)
	}
	b, _ := os.ReadFile(out)
	var e ext.Event
	if json.Unmarshal(b, &e) != nil || e.Action != "branch.create" || e.Principal.Name != "alice" {
		t.Errorf("recorded %s", b)
	}
}

func TestLoad_ディレクトリがなければモジュールなし(t *testing.T) {
	mods, err := plugin.Load(filepath.Join(t.TempDir(), "none"), quiet())
	if err != nil || len(mods) != 0 {
		t.Errorf("%v %v", mods, err)
	}
	if _, err := plugin.Load("/proc/self/fd/0/x", quiet()); err == nil {
		t.Error("unreadable dir: want error")
	}
}

func TestLoad_他人が書き換えられるものや合図に応えないものは使わない(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "writable"), []byte("#!/bin/sh\nexit 0\n"), 0o777)
	_ = os.Chmod(filepath.Join(dir, "writable"), 0o777)
	_ = os.WriteFile(filepath.Join(dir, "imposter"), []byte("#!/bin/sh\necho hello\n"), 0o755)
	mods, err := plugin.Load(dir, quiet())
	if len(mods) != 0 || err == nil {
		t.Errorf("mods %v err %v", mods, err)
	}
}

type memStore map[string][]byte

func (m memStore) List(context.Context) (map[string][]byte, error) { return m, nil }
func (m memStore) Put(_ context.Context, k string, v []byte) error {
	m[k] = v
	return nil
}
func (m memStore) Delete(_ context.Context, k string) error {
	delete(m, k)
	return nil
}

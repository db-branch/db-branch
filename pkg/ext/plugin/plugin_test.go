package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
			BranchPolicy: twoMax{}, PrincipalResolver: tokens{}})
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

// ResolvePrincipal makes tokens a PrincipalResolver.
func (tokens) ResolvePrincipal(_ context.Context, p ext.Principal) (ext.Principal, error) {
	if p.ID == "token:revoked" {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	return p, nil
}

// twoMax allows branches from base "prod" only and shares with "team:" owners.
type twoMax struct{}

func (twoMax) CheckBranch(_ context.Context, _ ext.Principal, r ext.BranchRequest) error {
	if r.Base != "prod" || len(r.Existing) >= 2 {
		return ext.ErrForbidden
	}
	return nil
}

func (twoMax) CheckExtend(_ context.Context, _ ext.Principal, r ext.BranchRequest) error {
	if r.TTL > 2*time.Hour {
		return ext.ErrForbidden
	}
	return nil
}

func (twoMax) SharesBranches(_ context.Context, _ ext.Principal, owner string) bool {
	return strings.HasPrefix(owner, "team:")
}

type hello struct{}

func (hello) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	switch r.Path {
	case "/env":
		return ext.Response{Status: 200, Body: []byte(os.Getenv(r.RawQuery))}, nil
	case "/cwd":
		wd, err := os.Getwd()
		if err != nil {
			return ext.Response{}, err
		}
		return ext.Response{Status: 200, Body: []byte(wd)}, nil
	}
	return ext.Response{Status: 200, Body: []byte("hello " + r.Principal.Name)}, nil
}

// keeper writes what it is given into its store, so the test sees Start work.
type keeper struct{}

func (*keeper) Start(ctx context.Context, h ext.Host) error {
	if err := h.Store.Put(ctx, "started-on", []byte(h.Node)); err != nil {
		return err
	}
	as, ok := h.Store.(ext.AtomicStore)
	if !ok {
		return errors.New("remoteStore must implement ext.AtomicStore")
	}
	okPut, err := as.PutIfAbsent(ctx, "atomic-k", []byte("ok"))
	if err != nil || !okPut {
		return fmt.Errorf("PutIfAbsent 1st: %v %v", okPut, err)
	}
	if okPut2, err := as.PutIfAbsent(ctx, "atomic-k", []byte("no")); err != nil || okPut2 {
		return fmt.Errorf("PutIfAbsent 2nd: %v %v", okPut2, err)
	}
	if v, found, err := as.Get(ctx, "atomic-k"); err != nil || !found || string(v) != "ok" {
		return fmt.Errorf("Get: %q %v %v", v, found, err)
	}
	v, found, err := as.Take(ctx, "atomic-k")
	if err != nil || !found || string(v) != "ok" {
		return fmt.Errorf("Take 1st: %q %v %v", v, found, err)
	}
	if _, found2, err := as.Take(ctx, "atomic-k"); err != nil || found2 {
		return fmt.Errorf("Take 2nd: %v %v", found2, err)
	}
	return h.Store.Put(ctx, "atomic", v)
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

// trustedTempDir returns a temp dir with mode 0755 regardless of the test
// process umask (Ubuntu defaults to umask 0002, which makes t.TempDir() 0775).
func trustedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// moduleDir holds a launcher that starts this test binary as a module.
func moduleDir(t *testing.T, out string) string {
	dir := trustedTempDir(t)
	self, _ := os.Executable()
	script := "#!/bin/sh\nDBB_TEST_MODULE_OUT=" + out + " exec " + self + "\n"
	bin := filepath.Join(dir, "10-test")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("not a module"), 0o644)
	return dir
}

func asRoot(fs.FileInfo) (uint32, bool) { return 0, true }

func TestLoad_別プロセスのモジュールを起動し各ポートを呼べる(t *testing.T) {
	out := filepath.Join(t.TempDir(), "audit.jsonl")
	dir := moduleDir(t, out)
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
	if err != nil || len(mods) != 1 {
		t.Fatalf("Load = %v %v", mods, err)
	}
	m := mods[0]
	defer m.Close()
	if m.Info.Name != "test-module" || m.Module.Authenticator == nil || m.Module.Authorizer == nil || m.Module.AuditSink == nil ||
		m.Module.Router == nil || m.Module.Ports["mysql"] != 6033 || m.Module.Configurable == nil || len(m.Module.Settings) != 1 || !m.Module.RequiresLicense || len(m.Module.PublicPaths) != 1 || len(m.Module.LoginMethods) != 1 {
		t.Fatalf("info = %+v", m.Info)
	}
	if m.Module.PrincipalResolver == nil {
		t.Fatal("missing PrincipalResolver")
	}
	if _, err := m.Module.PrincipalResolver.ResolvePrincipal(ctx, ext.Principal{ID: "token:revoked"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("revoked resolve: %v", err)
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
	ep, ok := bp.(ext.BranchExtendPolicy)
	if !ok || ep.CheckExtend(ctx, p, ext.BranchRequest{Base: "prod", TTL: time.Hour}) != nil ||
		!errors.Is(ep.CheckExtend(ctx, p, ext.BranchRequest{Base: "prod", TTL: 3 * time.Hour}), ext.ErrForbidden) {
		t.Error("branch extend policy over RPC")
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
	if string(data["atomic"]) != "ok" {
		t.Errorf("atomic store over RPC failed: %v", data)
	}
	b, _ := os.ReadFile(out)
	var e ext.Event
	if json.Unmarshal(b, &e) != nil || e.Action != "branch.create" || e.Principal.Name != "alice" {
		t.Errorf("recorded %s", b)
	}
}

func TestLoad_ディレクトリがなければモジュールなし(t *testing.T) {
	if mods, err := plugin.Load("", nil); err != nil || len(mods) != 0 {
		t.Errorf("empty dir with nil log: %v %v", mods, err)
	}
	// Exercises production default Load (trustRoot="/", default fileOwner, nil logger).
	if mods, err := plugin.Load("/db-branch-nonexistent-modules-test-dir", nil); err != nil || len(mods) != 0 {
		t.Errorf("missing absolute dir via Load: %v %v", mods, err)
	}
	root := trustedTempDir(t)
	mods, err := plugin.LoadForTest(filepath.Join(root, "none"), root, asRoot, quiet())
	if err != nil || len(mods) != 0 {
		t.Errorf("%v %v", mods, err)
	}
	notDir := filepath.Join(root, "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.LoadForTest(filepath.Join(notDir, "x"), root, asRoot, quiet()); !errors.Is(err, plugin.ErrUntrusted) {
		t.Errorf("path through regular file: err=%v, want ErrUntrusted", err)
	}
}

func TestLoad_他人が書き換えられるものや合図に応えないものは使わない(t *testing.T) {
	t.Run("writable binary is not executed", func(t *testing.T) {
		for name, perm := range map[string]os.FileMode{"group-writable": 0o775, "other-writable": 0o757, "world-writable": 0o777} {
			t.Run(name, func(t *testing.T) {
				dir := trustedTempDir(t)
				marker := filepath.Join(t.TempDir(), "ran")
				bin := filepath.Join(dir, "writable")
				if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(bin, perm); err != nil {
					t.Fatal(err)
				}
				mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
				for _, m := range mods {
					m.Close()
				}
				if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "writable by group or others") {
					t.Fatalf("%s: mods=%d err=%v", name, len(mods), err)
				}
				if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("%s: writable binary was executed", name)
				}
			})
		}
	})

	t.Run("imposter fails handshake", func(t *testing.T) {
		dir := trustedTempDir(t)
		bin := filepath.Join(dir, "imposter")
		_ = os.WriteFile(bin, []byte("#!/bin/sh\necho hello\n"), 0o755)
		_ = os.Chmod(bin, 0o755)
		mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || err == nil || errors.Is(err, plugin.ErrUntrusted) {
			t.Errorf("imposter: mods=%v err=%v", mods, err)
		}
	})
}

func TestPluginLoad_シンボリックリンクと特殊ビットと非root所有および親ディレクトリ権限不正を拒否する(t *testing.T) {
	t.Run("symlink binary is refused", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		evil := filepath.Join(t.TempDir(), "evil.sh")
		if err := os.WriteFile(evil, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "audit.jsonl")
		dir := moduleDir(t, out)
		if err := os.Symlink(evil, filepath.Join(dir, "20-link")); err != nil {
			t.Fatal(err)
		}
		mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
		for _, m := range mods {
			defer m.Close()
		}
		if len(mods) != 1 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlink alongside valid module: mods=%d err=%v, want 1 mod and ErrUntrusted", len(mods), err)
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("symlinked binary was executed")
		}
	})

	t.Run("special mode bits on binary are refused", func(t *testing.T) {
		for name, bit := range map[string]os.FileMode{
			"setuid": os.ModeSetuid,
			"setgid": os.ModeSetgid,
			"sticky": os.ModeSticky,
		} {
			t.Run(name, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "audit.jsonl")
				dir := moduleDir(t, out)
				bin := filepath.Join(dir, "10-test")
				if err := os.Chmod(bin, bit|0o755); err != nil {
					t.Skipf("cannot set %s: %v", name, err)
				}
				fi, err := os.Stat(bin)
				if err != nil || fi.Mode()&bit == 0 {
					t.Skipf("filesystem cleared %s bit", name)
				}
				mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
				for _, m := range mods {
					m.Close()
				}
				if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "unsafe mode bits") {
					t.Fatalf("%s binary: mods=%d err=%v, want 0 mods and ErrUntrusted (unsafe mode bits)", name, len(mods), err)
				}
			})
		}
	})

	t.Run("binary without owner execute bit is refused", func(t *testing.T) {
		for name, perm := range map[string]os.FileMode{"other-exec": 0o601, "group-exec": 0o610} {
			t.Run(name, func(t *testing.T) {
				dir := trustedTempDir(t)
				bin := filepath.Join(dir, "10-weird")
				if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(bin, perm); err != nil {
					t.Fatal(err)
				}
				mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
				for _, m := range mods {
					m.Close()
				}
				if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not executable by owner") {
					t.Fatalf("%s: mods=%d err=%v, want 0 mods and ErrUntrusted (not executable by owner)", name, len(mods), err)
				}
			})
		}
	})

	t.Run("unreadable binary returns non-untrusted permission error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can open 0o010 files")
		}
		dir := trustedTempDir(t)
		bin := filepath.Join(dir, "10-unreadable")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o010); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(bin, 0o600) })
		mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || err == nil || errors.Is(err, plugin.ErrUntrusted) || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("unreadable binary: mods=%d err=%v, want permission error (not ErrUntrusted)", len(mods), err)
		}
	})

	t.Run("non-root binary owner is refused", func(t *testing.T) {
		owner := func(fi fs.FileInfo) (uint32, bool) {
			if fi.Name() == "10-test" {
				return 1000, true
			}
			return 0, true
		}
		out := filepath.Join(t.TempDir(), "audit.jsonl")
		dir := moduleDir(t, out)
		mods, err := plugin.LoadForTest(dir, dir, owner, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not owned by root") {
			t.Fatalf("non-root binary: mods=%d err=%v, want 0 mods and ErrUntrusted (not owned by root)", len(mods), err)
		}
	})

	t.Run("unknown owner is refused", func(t *testing.T) {
		unknown := func(fs.FileInfo) (uint32, bool) { return 0, false }
		out := filepath.Join(t.TempDir(), "audit.jsonl")
		dir := moduleDir(t, out)
		mods, err := plugin.LoadForTest(dir, dir, unknown, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
			t.Fatalf("unknown owner: mods=%d err=%v, want 0 mods and ErrUntrusted", len(mods), err)
		}
	})

	t.Run("default owner check refuses non-root files", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root")
		}
		out := filepath.Join(t.TempDir(), "audit.jsonl")
		dir := moduleDir(t, out)
		mods, err := plugin.LoadForTest(dir, dir, nil, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not owned by root") {
			t.Fatalf("default owner check: mods=%d err=%v, want 0 mods and ErrUntrusted", len(mods), err)
		}
	})

	t.Run("relative, traversal, and out-of-root paths are refused", func(t *testing.T) {
		for name, p := range map[string]string{
			"relative":  "relative/modules",
			"traversal": "/usr/lib/../modules",
		} {
			mods, err := plugin.Load(p, nil)
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
				t.Fatalf("%s path %q: mods=%d err=%v, want ErrUntrusted", name, p, len(mods), err)
			}
		}
		root := t.TempDir()
		other := t.TempDir()
		mods, err := plugin.LoadForTest(other, root, asRoot, quiet())
		for _, m := range mods {
			m.Close()
		}
		if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
			t.Fatalf("outside trust root: mods=%d err=%v, want ErrUntrusted", len(mods), err)
		}
	})

	t.Run("parent and ancestor dir checks refuse unsafe permissions, owner, type, and symlinks", func(t *testing.T) {
		makeNested := func(t *testing.T) (root, parent, dir string) {
			t.Helper()
			root = trustedTempDir(t)
			parent = filepath.Join(root, "parent")
			dir = filepath.Join(parent, "modules")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{parent, dir} {
				if err := os.Chmod(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			self, _ := os.Executable()
			out := filepath.Join(t.TempDir(), "audit.jsonl")
			script := "#!/bin/sh\nDBB_TEST_MODULE_OUT=" + out + " exec " + self + "\n"
			bin := filepath.Join(dir, "10-test")
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			return root, parent, dir
		}

		t.Run("valid nested hierarchy loads", func(t *testing.T) {
			root, _, dir := makeNested(t)
			mods, err := plugin.LoadForTest(dir, root, asRoot, quiet())
			for _, m := range mods {
				defer m.Close()
			}
			if err != nil || len(mods) != 1 {
				t.Fatalf("valid nested dir: mods=%d err=%v", len(mods), err)
			}
		})

		t.Run("group or world writable parent dir", func(t *testing.T) {
			for name, perm := range map[string]os.FileMode{"group-writable": 0o775, "other-writable": 0o757} {
				t.Run(name, func(t *testing.T) {
					root, _, dir := makeNested(t)
					if err := os.Chmod(dir, perm); err != nil {
						t.Fatal(err)
					}
					mods, err := plugin.LoadForTest(dir, root, asRoot, quiet())
					for _, m := range mods {
						m.Close()
					}
					if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "writable by group or others") {
						t.Fatalf("%s dir: mods=%d err=%v", name, len(mods), err)
					}
				})
			}
		})

		t.Run("group writable ancestor dir", func(t *testing.T) {
			root, parent, dir := makeNested(t)
			if err := os.Chmod(parent, 0o775); err != nil {
				t.Fatal(err)
			}
			mods, err := plugin.LoadForTest(dir, root, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "writable by group or others") {
				t.Fatalf("writable ancestor dir: mods=%d err=%v", len(mods), err)
			}
		})

		t.Run("symlink ancestor dir", func(t *testing.T) {
			root := trustedTempDir(t)
			realParent := filepath.Join(root, "real")
			if err := os.MkdirAll(filepath.Join(realParent, "modules"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realParent, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			mods, err := plugin.LoadForTest(filepath.Join(root, "link", "modules"), root, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
				t.Fatalf("symlink ancestor dir: mods=%d err=%v, want ErrUntrusted", len(mods), err)
			}
		})

		t.Run("non-root ancestor owner", func(t *testing.T) {
			root, _, dir := makeNested(t)
			owner := func(fi fs.FileInfo) (uint32, bool) {
				if fi.Name() == "parent" {
					return 1000, true
				}
				return 0, true
			}
			mods, err := plugin.LoadForTest(dir, root, owner, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not owned by root") {
				t.Fatalf("non-root ancestor dir: mods=%d err=%v", len(mods), err)
			}
		})

		t.Run("special mode bit on dir", func(t *testing.T) {
			root, _, dir := makeNested(t)
			if err := os.Chmod(dir, os.ModeSticky|0o755); err != nil {
				t.Skipf("cannot set sticky on dir: %v", err)
			}
			fi, err := os.Stat(dir)
			if err != nil || fi.Mode()&os.ModeSticky == 0 {
				t.Skip("filesystem cleared sticky bit on dir")
			}
			mods, err := plugin.LoadForTest(dir, root, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "unsafe mode bits") {
				t.Fatalf("sticky dir: mods=%d err=%v", len(mods), err)
			}
		})

		t.Run("setgid on root-owned dir is allowed like runtimemodule", func(t *testing.T) {
			root, _, dir := makeNested(t)
			if err := os.Chmod(dir, os.ModeSetgid|0o755); err != nil {
				t.Skipf("cannot set setgid on dir: %v", err)
			}
			fi, err := os.Stat(dir)
			if err != nil || fi.Mode()&os.ModeSetgid == 0 {
				t.Skip("filesystem cleared setgid bit on dir")
			}
			mods, err := plugin.LoadForTest(dir, root, asRoot, quiet())
			for _, m := range mods {
				defer m.Close()
			}
			if err != nil || len(mods) != 1 {
				t.Fatalf("setgid dir: mods=%d err=%v, want 1 mod and nil err", len(mods), err)
			}
		})

		t.Run("not a directory", func(t *testing.T) {
			root := trustedTempDir(t)
			f := filepath.Join(root, "regular-file")
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			mods, err := plugin.LoadForTest(f, root, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("file as dir: mods=%d err=%v", len(mods), err)
			}
		})

		t.Run("symlink dir", func(t *testing.T) {
			root, _, realDir := makeNested(t)
			linkDir := filepath.Join(root, "modules-link")
			if err := os.Symlink(realDir, linkDir); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{linkDir, linkDir + string(os.PathSeparator)} {
				mods, err := plugin.LoadForTest(p, root, asRoot, quiet())
				for _, m := range mods {
					m.Close()
				}
				if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
					t.Fatalf("symlink parent dir %q: mods=%d err=%v", p, len(mods), err)
				}
			}
			// Trust root itself being a symlink is also refused.
			mods, err := plugin.LoadForTest(linkDir, linkDir, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) {
				t.Fatalf("symlink trust root: mods=%d err=%v", len(mods), err)
			}
		})
	})

	t.Run("FIFO and unix socket entries do not block and are refused", func(t *testing.T) {
		t.Run("FIFO", func(t *testing.T) {
			dir := trustedTempDir(t)
			fifo := filepath.Join(dir, "10-fifo")
			if err := syscall.Mkfifo(fifo, 0o755); err != nil {
				t.Skipf("cannot create fifo: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
				for _, m := range mods {
					m.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("fifo entry: err=%v, want ErrUntrusted (not a regular file)", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Load blocked on FIFO entry")
			}
		})

		t.Run("unix socket", func(t *testing.T) {
			// Use a short directory under /tmp so the socket path fits in sun_path (104 bytes on macOS).
			dir, err := os.MkdirTemp("/tmp", "dbb-sock-")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			ln, err := net.Listen("unix", filepath.Join(dir, "10-sock"))
			if err != nil {
				t.Fatalf("cannot create unix socket: %v", err)
			}
			defer ln.Close()
			mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
			for _, m := range mods {
				m.Close()
			}
			if len(mods) != 0 || !errors.Is(err, plugin.ErrUntrusted) || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("socket entry: mods=%d err=%v, want ErrUntrusted (not a regular file)", len(mods), err)
			}
		})
	})
}

func TestPluginLoad_子プロセスへホスト環境変数を継承しない(t *testing.T) {
	t.Setenv("DB_BRANCH_HOST_SECRET", "top-secret-value")
	t.Setenv("PATH", "/custom/host/path:/usr/bin:/bin")
	out := filepath.Join(t.TempDir(), "audit.jsonl")
	dir := moduleDir(t, out)
	mods, err := plugin.LoadForTest(dir, dir, asRoot, quiet())
	if err != nil || len(mods) != 1 {
		t.Fatalf("Load = %v %v", mods, err)
	}
	defer mods[0].Close()

	resp, err := mods[0].Module.Handler.Serve(ctx, ext.Request{Path: "/env", RawQuery: "DB_BRANCH_HOST_SECRET"})
	if err != nil {
		t.Fatalf("Serve /env secret: %v", err)
	}
	if got := string(resp.Body); got != "" {
		t.Errorf("child inherited host secret env var: got %q, want empty", got)
	}

	resp, err = mods[0].Module.Handler.Serve(ctx, ext.Request{Path: "/env", RawQuery: "PATH"})
	if err != nil {
		t.Fatalf("Serve /env PATH: %v", err)
	}
	if got, want := string(resp.Body), "/usr/sbin:/usr/bin:/sbin:/bin"; got != want {
		t.Errorf("child PATH = %q, want %q", got, want)
	}

	resp, err = mods[0].Module.Handler.Serve(ctx, ext.Request{Path: "/cwd"})
	if err != nil {
		t.Fatalf("Serve /cwd: %v", err)
	}
	if got := string(resp.Body); got != "/" {
		t.Errorf("child working directory = %q, want /", got)
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

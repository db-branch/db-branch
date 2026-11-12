package svcids_test

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/svcids"
)

type fakeRun struct {
	calls []string
	out   map[string]string
	fail  string
}

func (f *fakeRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		return nil, errors.New("boom")
	}
	for prefix, out := range f.out {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func ids(u, g int) domain.ServiceIDs { return domain.ServiceIDs{UID: u, GID: g} }

func TestSetIDs_変わったidだけgroupmodとusermodに渡す(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to domain.ServiceIDs
		want     []string
	}{
		{"both", ids(103, 109), ids(109, 112), []string{"groupmod -g 112 db-branch", "usermod -u 109 -g 112 db-branch"}},
		{"uid only", ids(103, 112), ids(109, 112), []string{"usermod -u 109 db-branch"}},
		{"gid only", ids(109, 109), ids(109, 112), []string{"groupmod -g 112 db-branch", "usermod -g 112 db-branch"}},
		{"none", ids(1, 2), ids(1, 2), nil},
	} {
		r := &fakeRun{}
		if err := (&svcids.Host{Run: r}).SetIDs(context.Background(), tc.from, tc.to); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.calls, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, r.calls, tc.want)
		}
	}
}

func TestSetIDs_コマンドの失敗を返す(t *testing.T) {
	for _, fail := range []string{"groupmod", "usermod"} {
		r := &fakeRun{fail: fail}
		if err := (&svcids.Host{Run: r}).SetIDs(context.Background(), ids(1, 2), ids(3, 4)); err == nil {
			t.Errorf("%s failing: want error", fail)
		}
	}
}

func TestStopServices_エージェントとDBサーバーのユニットを止める(t *testing.T) {
	r := &fakeRun{out: map[string]string{"systemctl list-units": "db-branch-mysqld@a.service loaded active running x\n\ndb-branch-postgres@b.service loaded failed failed y\n"}}
	if err := (&svcids.Host{Run: r, Systemd: true}).StopServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl stop db-branch.service",
		"systemctl list-units --plain --no-legend --all --type=service db-branch-mysqld@*.service db-branch-postgres@*.service",
		"systemctl stop db-branch-mysqld@a.service", "systemctl stop db-branch-postgres@b.service"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("calls = %v", r.calls)
	}
}

func TestStopServices_systemdがなければ何もせず_失敗は返す(t *testing.T) {
	r := &fakeRun{}
	if err := (&svcids.Host{Run: r}).StopServices(context.Background()); err != nil || len(r.calls) != 0 {
		t.Errorf("no systemd: %v %v", err, r.calls)
	}
	for _, fail := range []string{"systemctl stop db-branch.service", "systemctl list-units", "systemctl stop db-branch-mysqld@a"} {
		r := &fakeRun{fail: fail, out: map[string]string{"systemctl list-units": "db-branch-mysqld@a.service loaded\n"}}
		if err := (&svcids.Host{Run: r, Systemd: true}).StopServices(context.Background()); err == nil {
			t.Errorf("%q failing: want error", fail)
		}
	}
}

type chowned struct {
	path     string
	uid, gid int
}

func TestReown_古いidのファイルだけを新しいidへ_リンク自体を対象にする(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	_ = os.Mkdir(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "f"), nil, 0o644)
	_ = os.Symlink("/nonexistent", filepath.Join(dir, "link"))
	me := ids(os.Getuid(), os.Getgid())

	var got []chowned
	h := &svcids.Host{Dirs: []string{dir, filepath.Join(dir, "absent")}, Chown: func(p string, u, g int) error {
		got = append(got, chowned{p, u, g})
		return nil
	}}
	if err := h.Reown(context.Background(), me, ids(70001, 70002)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 { // dir, sub, sub/f, link
		t.Fatalf("chowned %v", got)
	}
	for _, c := range got {
		if c.uid != 70001 || c.gid != 70002 {
			t.Errorf("%v", c)
		}
	}
	// Files of other owners are left alone; a half match changes only that id.
	got = nil
	if err := h.Reown(context.Background(), ids(me.UID+1, me.GID), ids(70001, 70002)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].uid != me.UID || got[0].gid != 70002 {
		t.Errorf("gid only: %v", got)
	}
	got = nil
	_ = h.Reown(context.Background(), ids(me.UID+1, me.GID+1), ids(1, 2))
	if len(got) != 0 {
		t.Errorf("nothing matches: %v", got)
	}
}

func TestReown_chownの失敗とglobとZFSマウントを扱う(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "db-branch-x")
	_ = os.Mkdir(run, 0o755)
	mount := t.TempDir()
	me := ids(os.Getuid(), os.Getgid())
	r := &fakeRun{out: map[string]string{"zfs list": "none\n" + mount + "\n-\n"}}
	var seen []string
	h := &svcids.Host{Run: r, Pool: "tank", Globs: []string{filepath.Join(dir, "db-branch*")}, Chown: func(p string, _, _ int) error {
		seen = append(seen, p)
		return nil
	}}
	if err := h.Reown(context.Background(), me, ids(70001, 70002)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{"zfs list -H -o mountpoint -r tank/db-branch"}) || len(seen) != 2 || seen[0] != run || seen[1] != mount {
		t.Errorf("calls %v seen %v", r.calls, seen)
	}
	h.Chown = func(string, int, int) error { return errors.New("denied") }
	if err := h.Reown(context.Background(), me, ids(1, 2)); err == nil {
		t.Error("chown failure: want error")
	}
	h.Run = &fakeRun{fail: "zfs"}
	if err := h.Reown(context.Background(), me, ids(1, 2)); err == nil {
		t.Error("zfs failure: want error")
	}
}

func TestReown_既定のchownは実ファイルを操作できる(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(f, nil, 0o600)
	me := ids(os.Getuid(), os.Getgid())
	// Same ids: nothing to change, but the walk and default Chown wiring run.
	if err := (&svcids.Host{Dirs: []string{f}}).Reown(context.Background(), me, me); err != nil {
		t.Fatal(err)
	}
	// An unreadable tree is an error.
	d := t.TempDir()
	sub := filepath.Join(d, "locked")
	_ = os.Mkdir(sub, 0o000)
	defer os.Chmod(sub, 0o755)
	if os.Getuid() != 0 {
		if err := (&svcids.Host{Dirs: []string{d}}).Reown(context.Background(), me, me); err == nil {
			t.Error("unreadable directory: want error")
		}
	}
}

func TestLocal_とアカウント検索(t *testing.T) {
	h := &svcids.Host{}
	if _, err := h.Local(context.Background()); err != nil {
		// Machines without the package have no db-branch account.
		if u, uerr := user.Lookup("db-branch"); uerr == nil {
			t.Errorf("account exists (%s) but Local failed: %v", u.Uid, err)
		}
	}
	if h.UserWithUID(0) != "root" {
		t.Error("uid 0 is root")
	}
	if h.UserWithUID(1999999) != "" || h.GroupWithGID(1999999) != "" {
		t.Error("unused ids are free")
	}
	if h.GroupWithGID(0) == "" {
		t.Error("gid 0 has a name")
	}
	h = &svcids.Host{
		LookupUserID:  func(string) (*user.User, error) { return &user.User{Username: "x"}, nil },
		LookupGroupID: func(string) (*user.Group, error) { return &user.Group{Name: "y"}, nil },
	}
	if h.UserWithUID(5) != "x" || h.GroupWithGID(5) != "y" {
		t.Error("injected lookups")
	}
}

func TestLocal_ユーザーとグループのidを読み_欠けや非数値は失敗する(t *testing.T) {
	okUser := func(string) (*user.User, error) { return &user.User{Uid: "109"}, nil }
	okGroup := func(string) (*user.Group, error) { return &user.Group{Gid: "112"}, nil }
	h := &svcids.Host{LookupUser: okUser, LookupGroup: okGroup}
	if got, err := h.Local(context.Background()); err != nil || got != ids(109, 112) {
		t.Errorf("%v %v", got, err)
	}
	missing := errors.New("unknown")
	for name, h := range map[string]*svcids.Host{
		"no user":    {LookupUser: func(string) (*user.User, error) { return nil, missing }, LookupGroup: okGroup},
		"no group":   {LookupUser: okUser, LookupGroup: func(string) (*user.Group, error) { return nil, missing }},
		"bad number": {LookupUser: func(string) (*user.User, error) { return &user.User{Uid: "x"}, nil }, LookupGroup: okGroup},
	} {
		if _, err := h.Local(context.Background()); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestOwnerCheck_別のuidが所有するディレクトリはErrOwnershipで拒否する(t *testing.T) {
	dir := t.TempDir()
	me := os.Getuid()
	if err := svcids.OwnerCheck(func() (int, error) { return me, nil })(dir); err != nil {
		t.Errorf("own directory: %v", err)
	}
	err := svcids.OwnerCheck(func() (int, error) { return me + 1, nil })(dir)
	if !errors.Is(err, domain.ErrOwnership) || !strings.Contains(err.Error(), "cluster status") {
		t.Errorf("foreign directory: %v", err)
	}
	if err := svcids.OwnerCheck(func() (int, error) { return 0, errors.New("no user") })(dir); err != nil {
		t.Errorf("unknown service user is left to the server: %v", err)
	}
	if err := svcids.OwnerCheck(func() (int, error) { return me + 1, nil })(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("missing path is left to the server: %v", err)
	}
}

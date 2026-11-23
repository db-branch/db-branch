package apt_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/apt"
)

var ctx = context.Background()

type fake struct {
	calls   []string
	answers map[string]string // command prefix -> output
	fail    map[string]int    // command prefix -> remaining failures
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	for prefix, n := range f.fail {
		if strings.HasPrefix(call, prefix) && n != 0 {
			f.fail[prefix] = n - 1
			return []byte(f.answers[prefix]), errors.New("exit 1")
		}
	}
	for prefix, out := range f.answers {
		if strings.HasPrefix(call, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func TestInstalled_入っているものだけを版つきで返す(t *testing.T) {
	f := &fake{answers: map[string]string{"dpkg-query": "db-branch-proxysql\tinstall ok installed\t1.0\ndb-branch-old\tdeinstall ok config-files\t0.9\n"}}
	got, err := apt.Packages{Run: f}.Installed(ctx, "db-branch-")
	if err != nil || len(got) != 1 || got["db-branch-proxysql"] != "1.0" {
		t.Errorf("Installed = %v %v", got, err)
	}
	f = &fake{fail: map[string]int{"dpkg-query": 1}}
	if got, err := (apt.Packages{Run: f}).Installed(ctx, "db-branch-"); err != nil || len(got) != 0 {
		t.Errorf("nothing matches: %v %v", got, err)
	}
}

func TestCandidate_知らなければ一覧を更新してもう一度見る(t *testing.T) {
	f := &fake{answers: map[string]string{"apt-cache policy": "db-branch-proxysql:\n  Installed: (none)\n  Candidate: 1.2\n"}}
	if v, err := (apt.Packages{Run: f}).Candidate(ctx, "db-branch-proxysql"); err != nil || v != "1.2" || len(f.calls) != 1 {
		t.Errorf("Candidate = %q %v %v", v, err, f.calls)
	}
	f = &fake{answers: map[string]string{"apt-cache policy": "x:\n  Candidate: (none)\n"}}
	if v, err := (apt.Packages{Run: f}).Candidate(ctx, "x"); err != nil || v != "" || !strings.Contains(strings.Join(f.calls, ";"), "apt-get update") {
		t.Errorf("none: %q %v %v", v, err, f.calls)
	}
	f = &fake{answers: map[string]string{"apt-cache policy": "N: Unable to locate package x\n"}}
	if v, _ := (apt.Packages{Run: f}).Candidate(ctx, "x"); v != "" {
		t.Errorf("unknown: %q", v)
	}
	f = &fake{fail: map[string]int{"apt-cache": 1}}
	if _, err := (apt.Packages{Run: f}).Candidate(ctx, "x"); err == nil {
		t.Error("apt-cache failure: want error")
	}
	f = &fake{answers: map[string]string{"apt-cache policy": "x:\n"}, fail: map[string]int{"apt-get update": 1}}
	if _, err := (apt.Packages{Run: f}).Candidate(ctx, "x"); err == nil {
		t.Error("update failure: want error")
	}
}

func TestInstallとRemove_systemdの一時ユニットで走らせる(t *testing.T) {
	f := &fake{}
	p := apt.Packages{Run: f, Systemd: yes()}
	if err := p.Install(ctx, "db-branch-proxysql", "1.0"); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove(ctx, "db-branch-proxysql"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemd-run --quiet --wait --collect --description=db-branch module change --setenv=DEBIAN_FRONTEND=noninteractive apt-get -y -q -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=60 install --allow-downgrades db-branch-proxysql=1.0",
		"systemd-run --quiet --wait --collect --description=db-branch module change --setenv=DEBIAN_FRONTEND=noninteractive apt-get -y -q -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=60 remove db-branch-proxysql",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestInstall_失敗したら一覧を更新して一度だけやり直す(t *testing.T) {
	f := &fake{fail: map[string]int{"env": 1}}
	p := apt.Packages{Run: f, Systemd: no()}
	if err := p.Install(ctx, "db-branch-proxysql", "1.1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || !strings.HasPrefix(f.calls[1], "apt-get update") || !strings.HasPrefix(f.calls[2], "env DEBIAN_FRONTEND=noninteractive apt-get") {
		t.Errorf("calls %v", f.calls)
	}
	f = &fake{fail: map[string]int{"env": -1, "apt-get update": 1}}
	if err := (apt.Packages{Run: f, Systemd: no()}).Install(ctx, "x", "1"); err == nil {
		t.Error("want error")
	}
	f = &fake{fail: map[string]int{"env": -1}}
	if err := (apt.Packages{Run: f, Systemd: no()}).Install(ctx, "x", "1"); err == nil {
		t.Error("want error")
	}
	_ = apt.Packages{Run: f}.Remove(ctx, "x") // the machine decides systemd or not
}

func TestSearch_名前で探して説明を返す(t *testing.T) {
	f := &fake{answers: map[string]string{"apt-cache search": "db-branch-proxysql - ProxySQL routing for db-branch\ndb-branchx - other\n"}}
	got, err := apt.Packages{Run: f}.Search(ctx, "db-branch-")
	if err != nil || len(got) != 1 || got["db-branch-proxysql"] != "ProxySQL routing for db-branch" {
		t.Errorf("Search = %v %v", got, err)
	}
	if f.calls[0] != "apt-cache search --names-only ^db-branch-" {
		t.Errorf("call %q", f.calls[0])
	}
	f = &fake{fail: map[string]int{"apt-cache": 1}}
	if _, err := (apt.Packages{Run: f}).Search(ctx, "db-branch-"); err == nil {
		t.Error("want error")
	}
}

func TestRefresh_リポジトリの一覧だけを更新し_無ければ全体を更新する(t *testing.T) {
	list := filepath.Join(t.TempDir(), "db-branch.list")
	if err := os.WriteFile(list, []byte("deb x y z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	if err := (apt.Packages{Run: f, RepoList: list}).Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	want := "apt-get update -q -o Dir::Etc::sourcelist=" + list + " -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0"
	if f.calls[0] != want {
		t.Errorf("call %q, want %q", f.calls[0], want)
	}
	f = &fake{}
	if err := (apt.Packages{Run: f, RepoList: list + ".missing"}).Refresh(ctx); err != nil || f.calls[0] != "apt-get update -q" {
		t.Errorf("fallback: %v %v", f.calls, err)
	}
	f = &fake{fail: map[string]int{"apt-get update": 1}}
	if err := (apt.Packages{Run: f}).Refresh(ctx); err == nil {
		t.Error("want error")
	}
}

const madison = ` db-branch | 1.2.0 | https://repo stable/main amd64 Packages
 db-branch | 1.2.0 | https://mirror stable/main amd64 Packages
 db-branch | 1.1.0 | https://repo stable/main amd64 Packages
`

const show = `Package: db-branch-auth
Version: 1.2.0
Depends: tar, db-branch (>= 1.2.0), foo | bar

Package: db-branch-auth
Version: 1.1.0
Pre-Depends: db-branch (>> 1.0.0)

Package: db-branch-auth
Version: 1.0.0
Description: no dependency line
 continued db-branch (>= 9)
`

func TestVersions_提供される版と必要なdb_branchを返す(t *testing.T) {
	f := &fake{answers: map[string]string{"apt-cache madison": madison, "apt-cache show": show}}
	got, err := apt.Packages{Run: f}.Versions(ctx, "db-branch-auth")
	want := []domain.PackageVersion{{Version: "1.2.0", Requires: "1.2.0"}, {Version: "1.1.0", Requires: "1.0.0"}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Versions = %+v %v", got, err)
	}
	f = &fake{}
	if got, err := (apt.Packages{Run: f}).Versions(ctx, "nope"); err != nil || len(got) != 0 || len(f.calls) != 1 {
		t.Errorf("unknown package: %v %v %v", got, err, f.calls)
	}
	f = &fake{answers: map[string]string{"apt-cache madison": madison}, fail: map[string]int{"apt-cache show": 1}}
	if _, err := (apt.Packages{Run: f}).Versions(ctx, "x"); err == nil {
		t.Error("show failure: want error")
	}
	f = &fake{fail: map[string]int{"apt-cache madison": 1}}
	if _, err := (apt.Packages{Run: f}).Versions(ctx, "x"); err == nil {
		t.Error("madison failure: want error")
	}
}

func TestUpgrade_入っている版より新しいものだけを入れる(t *testing.T) {
	f := &fake{}
	p := apt.Packages{Run: f, Systemd: yes()}
	if err := p.Upgrade(ctx, map[string]string{"db-branch-auth": "1.2.0", "db-branch": "1.2.0", "db-branch-audit": "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	want := "systemd-run --quiet --wait --collect --description=db-branch module change --setenv=DEBIAN_FRONTEND=noninteractive apt-get -y -q -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=60 install --only-upgrade db-branch=1.2.0 db-branch-audit=1.1.0 db-branch-auth=1.2.0"
	if len(f.calls) != 1 || f.calls[0] != want {
		t.Errorf("calls %v", f.calls)
	}
	// A failure refreshes the lists once and tries again.
	f = &fake{fail: map[string]int{"env": 1}}
	if err := (apt.Packages{Run: f, Systemd: no()}).Upgrade(ctx, map[string]string{"db-branch": "1.2.0"}); err != nil || len(f.calls) != 3 {
		t.Errorf("retry: %v %v", err, f.calls)
	}
	f = &fake{fail: map[string]int{"env": -1}}
	if err := (apt.Packages{Run: f, Systemd: no()}).Upgrade(ctx, map[string]string{"db-branch": "1.2.0"}); err == nil {
		t.Error("want error")
	}
}

// overlap is a Runner that notes whether two commands ever ran at once.
type overlap struct {
	running, peak atomic.Int32
}

func (o *overlap) Run(context.Context, string, ...string) ([]byte, error) {
	n := o.running.Add(1)
	for p := o.peak.Load(); n > p && !o.peak.CompareAndSwap(p, n); p = o.peak.Load() {
	}
	time.Sleep(20 * time.Millisecond)
	o.running.Add(-1)
	return nil, nil
}

func TestAptLock_moduleSyncとupdateのdpkgを同時に実行しない(t *testing.T) {
	o := &overlap{}
	// Two Packages values, as module sync and the rolling update each hold one.
	a, b := apt.Packages{Run: o, Systemd: no()}, apt.Packages{Run: o, Systemd: no()}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = a.Install(ctx, "db-branch-x", "1") }()
		go func() { defer wg.Done(); _ = b.Upgrade(ctx, map[string]string{"db-branch": "1.2.0"}) }()
	}
	wg.Wait()
	if o.peak.Load() != 1 {
		t.Errorf("%d package changes ran at once", o.peak.Load())
	}
}

type blockingAptRunner struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
}

func (b *blockingAptRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "apt-cache" {
		return []byte("x:\n  Candidate: (none)\n"), nil
	}
	b.calls.Add(1)
	n := b.running.Add(1)
	for p := b.peak.Load(); n > p && !b.peak.CompareAndSwap(p, n); p = b.peak.Load() {
	}
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	b.running.Add(-1)
	return nil, errors.New("exit 1")
}

func TestAptLock_aptGetUpdateやRefreshも同時に実行せずキャンセルで待たない(t *testing.T) {
	r := &blockingAptRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	p := apt.Packages{Run: r, Systemd: no(), RepoList: filepath.Join(t.TempDir(), "missing.list")}

	done1 := make(chan error, 1)
	go func() { done1 <- p.Refresh(ctx) }()
	<-r.started

	// A second apt-get caller with a canceled context must not wait for the lock or start apt-get.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := p.Install(cctx, "db-branch-proxysql", "1.0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Install while locked with expired ctx = %v, want DeadlineExceeded", err)
	}
	// Candidate (which falls back to apt-get update) must also serialize on the same lock.
	done2 := make(chan error, 1)
	go func() { _, err := p.Candidate(ctx, "db-branch-proxysql"); done2 <- err }()
	time.Sleep(30 * time.Millisecond)
	if r.calls.Load() != 1 {
		t.Fatalf("calls while Refresh holds lock = %d, want 1", r.calls.Load())
	}

	close(r.release)
	<-done1
	<-done2
	if r.peak.Load() != 1 {
		t.Errorf("peak concurrent apt-get = %d, want 1", r.peak.Load())
	}

	// Already-canceled context fails deterministically even when the lock is free.
	canceledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	for range 20 {
		if err := p.Refresh(canceledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Refresh with canceled ctx = %v, want Canceled", err)
		}
	}
}

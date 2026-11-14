package runtimestore_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimestore"
)

var (
	ctx     = context.Background()
	mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}
	mysql80 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
	pg17    = domain.RuntimeRef{Engine: domain.Postgres, Version: "17.2"}
)

// archive builds a tar.xz shaped like the upstream MySQL tarball: one top-level directory.
func archive(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	const topDir = "mysql-8.4.13-linux-glibc2.28-x86_64-minimal"
	top := filepath.Join(src, topDir, "bin")
	if err := os.MkdirAll(top, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "mysqld"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rt.tar.xz")
	if b, err := exec.Command("tar", "-cJf", out, "-C", src, topDir).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	return out
}

func newStore(t *testing.T) (runtimestore.Store, string) {
	dir := filepath.Join(t.TempDir(), "runtimes")
	return runtimestore.Store{Dir: dir, Run: execx.Runner{}}, dir
}

func TestInstall_最上位ディレクトリを外してバージョンごとの場所に展開する(t *testing.T) {
	s, dir := newStore(t)

	if err := s.Install(ctx, mysql84, []string{archive(t)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "mysql", "8.4.13", "bin", "mysqld"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("mysqld should stay executable")
	}
	if ok, _ := s.Has(ctx, mysql84); !ok {
		t.Error("Has should be true after Install")
	}
	if got := s.Path(mysql84); got != filepath.Join(dir, "mysql", "8.4.13") {
		t.Errorf("Path = %q", got)
	}
}

func TestInstall_展開に失敗したら何も残さない(t *testing.T) {
	s, dir := newStore(t)
	bad := filepath.Join(t.TempDir(), "bad.tar.xz")
	_ = os.WriteFile(bad, []byte("not an archive"), 0o644)

	if err := s.Install(ctx, mysql84, []string{bad}); err == nil {
		t.Fatal("want error, got nil")
	}
	if ok, _ := s.Has(ctx, mysql84); ok {
		t.Error("Has should be false")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "mysql"))
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

func TestInstall_既にあれば上書きしない(t *testing.T) {
	s, _ := newStore(t)
	a := archive(t)
	_ = s.Install(ctx, mysql84, []string{a})

	if err := s.Install(ctx, mysql84, []string{a}); !errors.Is(err, domain.ErrRuntimeInstalled) {
		t.Fatalf("error = %v, want ErrRuntimeInstalled", err)
	}
}

func TestInstall_置き場を作れなければ失敗する(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	s := runtimestore.Store{Dir: file, Run: execx.Runner{}}

	if err := s.Install(ctx, mysql84, []string{archive(t)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestList_インストール済みのものだけを並べて返す(t *testing.T) {
	s, dir := newStore(t)
	a := archive(t)
	for _, r := range []domain.RuntimeRef{pg17, mysql84, mysql80} {
		if err := s.Install(ctx, r, []string{a}); err != nil {
			t.Fatal(err)
		}
	}
	// Not runtimes: a half-finished staging dir and an unrelated directory.
	_ = os.MkdirAll(filepath.Join(dir, "mysql", ".staging-123"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "mysql", "9.9.9"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "oracle", "1.0"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "README"), nil, 0o644)

	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.RuntimeRef{mysql80, mysql84, pg17}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestList_まだ何もなければ空(t *testing.T) {
	s, _ := newStore(t)

	got, err := s.List(ctx)
	if err != nil || len(got) != 0 {
		t.Errorf("List = %v, %v", got, err)
	}
}

func TestList_置き場が読めなければ失敗する(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	s := runtimestore.Store{Dir: file, Run: execx.Runner{}}

	if _, err := s.List(ctx); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestRemove_ランタイムを削除する(t *testing.T) {
	s, dir := newStore(t)
	_ = s.Install(ctx, mysql84, []string{archive(t)})

	if err := s.Remove(ctx, mysql84); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mysql", "8.4.13")); !os.IsNotExist(err) {
		t.Errorf("directory should be gone: %v", err)
	}
}

func TestRemove_なければ失敗する(t *testing.T) {
	s, _ := newStore(t)

	if err := s.Remove(ctx, mysql84); !errors.Is(err, domain.ErrRuntimeNotInstalled) {
		t.Fatalf("error = %v, want ErrRuntimeNotInstalled", err)
	}
}

func TestInstall_置き場に書き込めなければ失敗する(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, dir := newStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "mysql"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "mysql"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "mysql"), 0o755) })

	if err := s.Install(ctx, mysql84, []string{archive(t)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestInstall_アーカイブが目印のファイル名を使っていたら失敗する(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "top", ".db-branch-runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(t.TempDir(), "rt.tar.xz")
	if b, err := exec.Command("tar", "-cJf", a, "-C", src, "top").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	s, _ := newStore(t)

	if err := s.Install(ctx, mysql84, []string{a}); err == nil {
		t.Fatal("want error, got nil")
	}
	if ok, _ := s.Has(ctx, mysql84); ok {
		t.Error("Has should be false")
	}
}

func TestInstall_エンジンごとの後処理を展開したディレクトリに対して行う(t *testing.T) {
	var seen string
	s, dir := newStore(t)
	s.Fixups = map[domain.Engine]func(string) error{
		domain.MySQL: func(d string) error {
			if _, err := os.Stat(filepath.Join(d, "bin", "mysqld")); err != nil {
				return err
			}
			seen = d
			return os.WriteFile(filepath.Join(d, "fixed"), nil, 0o644)
		},
	}

	if err := s.Install(ctx, mysql84, []string{archive(t)}); err != nil {
		t.Fatal(err)
	}
	if seen == "" {
		t.Fatal("fixup was not called")
	}
	if _, err := os.Stat(filepath.Join(dir, "mysql", "8.4.13", "fixed")); err != nil {
		t.Errorf("fixup result should be part of the installed runtime: %v", err)
	}
}

func TestInstall_後処理に失敗したら何も残さない(t *testing.T) {
	s, dir := newStore(t)
	s.Fixups = map[domain.Engine]func(string) error{
		domain.MySQL: func(string) error { return errors.New("no libaio") },
	}

	if err := s.Install(ctx, mysql84, []string{archive(t)}); err == nil || !strings.Contains(err.Error(), "no libaio") {
		t.Fatalf("error = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "mysql"))
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

func TestInstall_同時に入れても片方はインストール済みとして終わる(t *testing.T) {
	s, _ := newStore(t)
	a := archive(t)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- s.Install(ctx, mysql84, []string{a}) }()
	}
	ok := 0
	for i := 0; i < 4; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, domain.ErrRuntimeInstalled):
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d installs succeeded, want 1", ok)
	}
}

func TestInstall_debはそのまま展開し複数をひとつにまとめる(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not available")
	}
	mkdeb := func(name, file string) string {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, "DEBIAN"), 0o755)
		_ = os.WriteFile(filepath.Join(root, "DEBIAN", "control"), []byte("Package: "+name+"\nVersion: 1\nArchitecture: all\nMaintainer: t <t@t>\nDescription: t\n"), 0o644)
		_ = os.MkdirAll(filepath.Join(root, filepath.Dir(file)), 0o755)
		_ = os.WriteFile(filepath.Join(root, file), []byte("x"), 0o755)
		out := filepath.Join(t.TempDir(), name+".deb")
		if b, err := exec.Command("dpkg-deb", "--build", "--root-owner-group", root, out).CombinedOutput(); err != nil {
			t.Fatalf("dpkg-deb: %v %s", err, b)
		}
		return out
	}
	s, dir := newStore(t)
	err := s.Install(ctx, pg17, []string{mkdeb("server", "usr/lib/postgresql/17/bin/postgres"), mkdeb("libpq", "usr/lib/x86_64-linux-gnu/libpq.so.5")})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"usr/lib/postgresql/17/bin/postgres", "usr/lib/x86_64-linux-gnu/libpq.so.5"} {
		if _, err := os.Stat(filepath.Join(dir, "postgres", "17.2", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := s.Install(ctx, mysql80, []string{"/nope"}); err == nil {
		t.Error("missing archive: want error")
	}
}

func TestInstall_tar_gz_の旧版も最上位ディレクトリを外して展開する(t *testing.T) {
	s, dir := newStore(t)
	src := t.TempDir()
	const topDir = "mysql-5.7.44-linux-glibc2.12-x86_64"
	if err := os.MkdirAll(filepath.Join(src, topDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, topDir, "bin", "mysqld"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rt.tar.gz")
	if b, err := exec.Command("tar", "-czf", out, "-C", src, topDir).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	ref := domain.RuntimeRef{Engine: domain.MySQL, Version: "5.7.44"}
	if err := s.Install(ctx, ref, []string{out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mysql", "5.7.44", "bin", "mysqld")); err != nil {
		t.Fatal(err)
	}
}

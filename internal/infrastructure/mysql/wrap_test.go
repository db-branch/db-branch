package mysql

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// legacyRuntime builds the layout of an old MySQL tarball: binaries without an
// RPATH, and the compat links the fixup made in lib/private.
func legacyRuntime(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"bin", filepath.Join("lib", "private")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []string{"mysqld", "mysql", "mysqladmin"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\necho \"$0 args=$* ld=$LD_LIBRARY_PATH\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/nonexistent/libaio.so.1t64", filepath.Join(dir, "lib", "private", "libaio.so.1")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func needs(m map[string][]string) func(string) ([]string, error) {
	return func(p string) ([]string, error) { return m[filepath.Base(p)], nil }
}

func TestWrapLegacy_RPATHのない旧版の実行ファイルは_互換ライブラリを見つける起動ラッパーにする(t *testing.T) {
	dir := legacyRuntime(t)
	err := WrapLegacy(dir, needs(map[string][]string{"mysqld": {"libaio.so.1", "libc.so.6"}, "mysql": {"libtinfo.so.5"}, "mysqladmin": {"libc.so.6"}}), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"mysqld"} {
		if _, err := os.Stat(filepath.Join(dir, "bin", b+".real")); err != nil {
			t.Errorf("%s.real: %v", b, err)
		}
	}
	// A binary that needs nothing of the compat links stays as it is.
	if _, err := os.Stat(filepath.Join(dir, "bin", "mysqladmin.real")); err == nil {
		t.Error("mysqladmin needs no compat library and must not be wrapped")
	}
	out, err := exec.Command(filepath.Join(dir, "bin", "mysqld"), "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper: %v %s", err, out)
	}
	if !strings.Contains(string(out), "args=--version") || !strings.Contains(string(out), "/../lib/private") {
		t.Errorf("the wrapper passes the arguments and puts lib/private on the library path: %s", out)
	}
}

func TestWrapLegacy_互換リンクの名前を使わない実行ファイルや_RPATHのある実行ファイルは包まない(t *testing.T) {
	dir := legacyRuntime(t)
	err := WrapLegacy(dir, needs(map[string][]string{"mysqld": {"libaio.so.1"}, "mysql": {"libtinfo.so.5"}}), func(p string) bool { return filepath.Base(p) == "mysqld" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "mysqld.real")); err == nil {
		t.Error("a binary with an RPATH finds lib/private itself")
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "mysql.real")); err == nil {
		t.Error("libtinfo.so.5 has no link in lib/private here, so nothing to wrap for")
	}
}

func TestWrapLegacy_もう包んであれば二重に包まない(t *testing.T) {
	dir := legacyRuntime(t)
	n := needs(map[string][]string{"mysqld": {"libaio.so.1"}})
	no := func(string) bool { return false }
	if err := WrapLegacy(dir, n, no); err != nil {
		t.Fatal(err)
	}
	if err := WrapLegacy(dir, n, no); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "mysqld.real.real")); err == nil {
		t.Error("wrapped twice")
	}
	out, _ := exec.Command(filepath.Join(dir, "bin", "mysqld")).CombinedOutput()
	if !strings.Contains(string(out), "ld=") {
		t.Errorf("still runs: %s", out)
	}
}

func TestWrapLegacy_bin_がなければ何もしない(t *testing.T) {
	if err := WrapLegacy(t.TempDir(), needs(nil), func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
}

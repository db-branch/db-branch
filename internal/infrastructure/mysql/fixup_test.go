package mysql_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFixLibaio_t64版しかなければランタイム内にリンクを張る(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libaio.so.1t64"))

	if err := mysql.FixLibaio(rt, []string{"/nonexistent", sys}); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(rt, "lib", "private", "libaio.so.1"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(sys, "libaio.so.1t64") {
		t.Errorf("link target = %q", target)
	}
}

func TestFixLibaio_システムに元の名前があれば何もしない(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libaio.so.1"))
	touch(t, filepath.Join(sys, "libaio.so.1t64"))

	if err := mysql.FixLibaio(rt, []string{sys}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rt, "lib", "private", "libaio.so.1")); !os.IsNotExist(err) {
		t.Errorf("no link expected, got %v", err)
	}
}

func TestFixLibaio_ランタイムが同梱していれば何もしない(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(rt, "lib", "private", "libaio.so.1"))

	if err := mysql.FixLibaio(rt, []string{sys}); err != nil {
		t.Fatal(err)
	}
}

func TestFixLibaio_どこにもなければ入れるべきパッケージを示して失敗する(t *testing.T) {
	err := mysql.FixLibaio(t.TempDir(), []string{t.TempDir()})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if want := "libaio1t64"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err, want)
	}
}

func TestFixLibaio_リンクを作れなければ失敗する(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libaio.so.1t64"))
	touch(t, filepath.Join(rt, "lib")) // a file where the lib directory should be

	if err := mysql.FixLibaio(rt, []string{sys}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestSystemLibDirs_このアーキテクチャのライブラリの場所を含む(t *testing.T) {
	dirs := mysql.SystemLibDirs("amd64")
	if len(dirs) == 0 || dirs[0] != "/usr/lib/x86_64-linux-gnu" {
		t.Errorf("amd64 dirs = %v", dirs)
	}
	if dirs := mysql.SystemLibDirs("arm64"); dirs[0] != "/usr/lib/aarch64-linux-gnu" {
		t.Errorf("arm64 dirs = %v", dirs)
	}
}

func TestFixNcurses_ncurses5が要る古いクライアントには6へのリンクを張る(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libtinfo.so.6"))
	touch(t, filepath.Join(sys, "libncurses.so.6"))
	needed := []string{"libncurses.so.5", "libtinfo.so.5", "libc.so.6"}
	if err := mysql.FixNcurses(rt, needed, []string{sys}); err != nil {
		t.Fatal(err)
	}
	for _, lib := range []string{"libncurses.so.5", "libtinfo.so.5"} {
		target, err := os.Readlink(filepath.Join(rt, "lib", "private", lib))
		if err != nil || filepath.Dir(target) != sys {
			t.Errorf("%s -> %q %v", lib, target, err)
		}
	}
}

func TestFixNcurses_要らないかシステムにあれば何もしない(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libtinfo.so.5"))
	if err := mysql.FixNcurses(rt, []string{"libtinfo.so.5", "libc.so.6"}, []string{sys}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rt, "lib", "private")); !os.IsNotExist(err) {
		t.Errorf("no link expected: %v", err)
	}
	if err := mysql.FixNcurses(rt, []string{"libncurses.so.5"}, []string{t.TempDir()}); err == nil || !strings.Contains(err.Error(), "libncurses") {
		t.Errorf("nothing to link to: %v", err)
	}
	blocked := t.TempDir()
	touch(t, filepath.Join(blocked, "lib"))
	touch(t, filepath.Join(sys, "libncurses.so.6"))
	if err := mysql.FixNcurses(blocked, []string{"libncurses.so.5"}, []string{sys}); err == nil {
		t.Error("cannot link")
	}
}

func TestNeededLibs_実行ファイルが要る共有ライブラリを返す(t *testing.T) {
	self, _ := os.Executable()
	libs, err := mysql.NeededLibs(self)
	if err != nil {
		t.Skipf("not an ELF executable here: %v", err)
	}
	_ = libs // a static Go test binary may need none
	if _, err := mysql.NeededLibs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file")
	}
}

func TestFixup_mysqlとmysqldの両方を見て直す(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libaio.so.1"))
	touch(t, filepath.Join(sys, "libtinfo.so.6"))
	needs := map[string][]string{"mysql": {"libtinfo.so.5"}, "mysqld": {"libaio.so.1"}}
	fix := mysql.Fixup([]string{sys}, func(bin string) ([]string, error) { return needs[filepath.Base(bin)], nil })
	if err := fix(rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(rt, "lib", "private", "libtinfo.so.5")); err != nil {
		t.Error(err)
	}
	fail := mysql.Fixup([]string{t.TempDir()}, func(string) ([]string, error) { return nil, nil })
	if err := fail(t.TempDir()); err == nil {
		t.Error("libaio missing everywhere")
	}
}

func TestDropDebug_デバッグ版のサーバーとプラグインを消し通常版は残す(t *testing.T) {
	rt := t.TempDir()
	for _, p := range []string{"bin/mysqld", "bin/mysqld-debug", "bin/mysql", "lib/plugin/auth_socket.so", "lib/plugin/debug/auth_socket.so"} {
		touch(t, filepath.Join(rt, p))
	}
	if err := mysql.DropDebug(rt); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/mysqld-debug", "lib/plugin/debug"} {
		if _, err := os.Lstat(filepath.Join(rt, p)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed: %v", p, err)
		}
	}
	for _, p := range []string{"bin/mysqld", "bin/mysql", "lib/plugin/auth_socket.so"} {
		if _, err := os.Stat(filepath.Join(rt, p)); err != nil {
			t.Errorf("%s should be kept: %v", p, err)
		}
	}
}

func TestDropDebug_デバッグ版がない最小版では何もしない(t *testing.T) {
	rt := t.TempDir()
	touch(t, filepath.Join(rt, "bin", "mysqld"))
	if err := mysql.DropDebug(rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rt, "bin", "mysqld")); err != nil {
		t.Error(err)
	}
}

func TestFixup_デバッグ版を消してから直す(t *testing.T) {
	rt, sys := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(sys, "libaio.so.1"))
	touch(t, filepath.Join(rt, "bin", "mysqld-debug"))
	fix := mysql.Fixup([]string{sys}, func(string) ([]string, error) { return nil, nil })
	if err := fix(rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rt, "bin", "mysqld-debug")); !os.IsNotExist(err) {
		t.Errorf("mysqld-debug should be removed: %v", err)
	}
}

func TestDropDebug_消せなければ失敗しFixupも止まる(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove from a read-only directory")
	}
	rt := t.TempDir()
	touch(t, filepath.Join(rt, "bin", "mysqld-debug"))
	bin := filepath.Join(rt, "bin")
	if err := os.Chmod(bin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bin, 0o755) })
	if err := mysql.DropDebug(rt); err == nil {
		t.Error("DropDebug: want error")
	}
	fix := mysql.Fixup([]string{t.TempDir()}, func(string) ([]string, error) { return nil, nil })
	if err := fix(rt); err == nil {
		t.Error("Fixup: want error")
	}
}

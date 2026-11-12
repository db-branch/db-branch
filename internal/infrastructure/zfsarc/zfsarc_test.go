package zfsarc_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/zfsarc"
)

func setup(t *testing.T, meminfo string) zfsarc.Config {
	t.Helper()
	dir := t.TempDir()
	c := zfsarc.Config{
		MemInfo:  filepath.Join(dir, "meminfo"),
		Param:    filepath.Join(dir, "zfs_arc_max"),
		Modprobe: filepath.Join(dir, "modprobe.d"),
	}
	_ = os.WriteFile(c.MemInfo, []byte(meminfo), 0o644)
	_ = os.WriteFile(c.Param, []byte("0\n"), 0o644)
	_ = os.MkdirAll(c.Modprobe, 0o755)
	return c
}

const meminfo = "MemTotal:       12288000 kB\nMemFree:         1000000 kB\n"

func TestDefaultMax_物理メモリの4分の1(t *testing.T) {
	c := setup(t, meminfo)
	got, err := c.DefaultMax()
	if err != nil {
		t.Fatal(err)
	}
	if got != 12288000*1024/4 {
		t.Errorf("DefaultMax = %d", got)
	}
}

func TestApply_実行中のカーネルと再起動後の設定の両方に書く(t *testing.T) {
	c := setup(t, meminfo)

	note, err := c.Apply(3 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.Param); strings.TrimSpace(string(b)) != "3221225472" {
		t.Errorf("param = %q", b)
	}
	b, _ := os.ReadFile(filepath.Join(c.Modprobe, "db-branch-zfs.conf"))
	if !strings.Contains(string(b), "options zfs zfs_arc_max=3221225472") {
		t.Errorf("modprobe = %q", b)
	}
	if !strings.Contains(note, "3.0 GiB") {
		t.Errorf("note = %q", note)
	}
}

func TestApply_管理者が別の設定で決めていれば上書きせずに知らせる(t *testing.T) {
	c := setup(t, meminfo)
	_ = os.WriteFile(filepath.Join(c.Modprobe, "zfs.conf"), []byte("options zfs zfs_arc_max=1073741824\n"), 0o644)

	note, err := c.Apply(3 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.Param); strings.TrimSpace(string(b)) != "0" {
		t.Errorf("param should be untouched: %q", b)
	}
	if !strings.Contains(note, "zfs.conf") {
		t.Errorf("note = %q", note)
	}
}

func Test読み書きできなければエラー(t *testing.T) {
	c := setup(t, "garbage")
	if _, err := c.DefaultMax(); err == nil {
		t.Error("garbage meminfo: want error")
	}
	c.MemInfo = filepath.Join(t.TempDir(), "missing")
	if _, err := c.DefaultMax(); err == nil {
		t.Error("missing meminfo: want error")
	}
	c = setup(t, meminfo)
	c.Param = filepath.Join(t.TempDir(), "no", "such")
	if _, err := c.Apply(1 << 30); err == nil {
		t.Error("unwritable param: want error")
	}
	c = setup(t, meminfo)
	c.Modprobe = filepath.Join(t.TempDir(), "missing-dir")
	if _, err := c.Apply(1 << 30); err == nil {
		t.Error("missing modprobe dir: want error")
	}
}

func Test数値でないMemTotalや書けない設定ファイルはエラー(t *testing.T) {
	c := setup(t, "MemTotal: lots kB\n")
	if _, err := c.DefaultMax(); err == nil {
		t.Error("non-numeric MemTotal: want error")
	}
	c = setup(t, meminfo)
	_ = os.MkdirAll(filepath.Join(c.Modprobe, "db-branch-zfs.conf", "x"), 0o755)
	if _, err := c.Apply(1 << 30); err == nil {
		t.Error("unwritable modprobe conf: want error")
	}
}

func TestCurrent_動いているカーネルの上限を読む(t *testing.T) {
	c := setup(t, meminfo)
	_ = os.WriteFile(c.Param, []byte("3221225472\n"), 0o644)
	if got, err := c.Current(); err != nil || got != 3<<30 {
		t.Errorf("Current = %d, %v", got, err)
	}
	_ = os.WriteFile(c.Param, []byte("many\n"), 0o644)
	if _, err := c.Current(); err == nil {
		t.Error("non-numeric: want error")
	}
	c.Param = filepath.Join(t.TempDir(), "missing")
	if _, err := c.Current(); err == nil {
		t.Error("missing: want error")
	}
}

func TestSet_管理者の設定があっても実行中と再起動後の両方に書く(t *testing.T) {
	c := setup(t, meminfo)
	if err := c.Set(2 << 30); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.Param); strings.TrimSpace(string(b)) != "2147483648" {
		t.Errorf("param = %q", b)
	}
	b, _ := os.ReadFile(filepath.Join(c.Modprobe, "db-branch-zfs.conf"))
	if !strings.Contains(string(b), "options zfs zfs_arc_max=2147483648") {
		t.Errorf("modprobe = %q", b)
	}
	// Setting it again replaces the earlier value.
	if err := c.Set(4 << 30); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(c.Modprobe, "db-branch-zfs.conf"))
	if strings.Count(string(b), "zfs_arc_max=") != 1 || !strings.Contains(string(b), "4294967296") {
		t.Errorf("modprobe = %q", b)
	}
}

func TestSet_書けなければエラー(t *testing.T) {
	c := setup(t, meminfo)
	c.Param = filepath.Join(t.TempDir(), "no", "such")
	if err := c.Set(1 << 30); err == nil {
		t.Error("unwritable param: want error")
	}
	c = setup(t, meminfo)
	_ = os.MkdirAll(filepath.Join(c.Modprobe, "db-branch-zfs.conf", "x"), 0o755)
	if err := c.Set(1 << 30); err == nil {
		t.Error("unwritable modprobe conf: want error")
	}
}

func TestConflict_ほかのmodprobe設定が上限を決めていれば知らせる(t *testing.T) {
	c := setup(t, meminfo)
	if got := c.Conflict(); got != "" {
		t.Errorf("no other file: %q", got)
	}
	_ = os.WriteFile(filepath.Join(c.Modprobe, "db-branch-zfs.conf"), []byte("options zfs zfs_arc_max=1\n"), 0o644)
	if got := c.Conflict(); got != "" {
		t.Errorf("our own file is not a conflict: %q", got)
	}
	_ = os.WriteFile(filepath.Join(c.Modprobe, "zfs.conf"), []byte("options zfs zfs_arc_max=1073741824\n"), 0o644)
	if got := c.Conflict(); !strings.HasSuffix(got, "zfs.conf") {
		t.Errorf("Conflict = %q", got)
	}
	c.Modprobe = filepath.Join(t.TempDir(), "missing")
	if got := c.Conflict(); got != "" {
		t.Errorf("unreadable directory: %q", got)
	}
}

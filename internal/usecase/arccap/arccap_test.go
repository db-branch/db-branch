package arccap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/usecase/arccap"
)

const gib = 1 << 30

type fakeSys struct {
	mem, cur uint64
	conflict string
	setErr   error
	memErr   error
	curErr   error
	sets     []uint64
}

func (f *fakeSys) MemTotal() (uint64, error) { return f.mem, f.memErr }
func (f *fakeSys) Current() (uint64, error)  { return f.cur, f.curErr }
func (f *fakeSys) Conflict() string          { return f.conflict }
func (f *fakeSys) Set(max uint64) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.cur = max
	f.sets = append(f.sets, max)
	return nil
}

func newCtl(f *fakeSys) *arccap.Controller {
	return &arccap.Controller{Sys: f, Initial: func() (uint64, error) { return 4 * gib, nil }}
}

func TestSync_設定が空なら何もしない(t *testing.T) {
	f := &fakeSys{mem: 16 * gib, cur: 4 * gib}
	if err := newCtl(f).Sync(""); err != nil || len(f.sets) != 0 {
		t.Errorf("err %v, sets %v", err, f.sets)
	}
}

func TestSync_割合やサイズをこのノードの値に直して適用し同じ値なら書き直さない(t *testing.T) {
	f := &fakeSys{mem: 16 * gib, cur: 4 * gib}
	c := newCtl(f)
	if err := c.Sync("50%"); err != nil {
		t.Fatal(err)
	}
	if err := c.Sync("50%"); err != nil {
		t.Fatal(err)
	}
	if len(f.sets) != 1 || f.sets[0] != 8*gib {
		t.Errorf("sets = %v", f.sets)
	}
	if err := c.Sync("2GB"); err != nil || f.cur != 2*gib {
		t.Errorf("cur = %d, %v", f.cur, err)
	}
}

func TestSync_空に戻すとinitで決めた値に戻す(t *testing.T) {
	f := &fakeSys{mem: 16 * gib, cur: 4 * gib}
	c := newCtl(f)
	_ = c.Sync("8GB")
	if err := c.Sync(""); err != nil || f.cur != 4*gib {
		t.Fatalf("cur = %d, %v", f.cur, err)
	}
	n := len(f.sets)
	_ = c.Sync("")
	if len(f.sets) != n {
		t.Errorf("restored twice: %v", f.sets)
	}
}

func TestSync_適用できない理由を返す(t *testing.T) {
	f := &fakeSys{mem: 4 * gib, cur: 1 * gib}
	c := newCtl(f)
	if err := c.Sync("nonsense"); err == nil {
		t.Error("bad syntax: want error")
	}
	if err := c.Sync("4GB"); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("too big for this node: %v", err)
	}
	f.memErr = errors.New("no meminfo")
	if err := c.Sync("1GB"); err == nil {
		t.Error("memory unknown: want error")
	}
	f.memErr, f.curErr = nil, errors.New("no zfs module")
	if err := c.Sync("1GB"); err == nil {
		t.Error("ZFS parameter unreadable: want error")
	}
	f.curErr, f.setErr = nil, errors.New("read-only")
	if err := c.Sync("2GB"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("write failed: %v", err)
	}
	if len(f.sets) != 0 {
		t.Errorf("nothing should be set: %v", f.sets)
	}
}

func TestSync_空に戻す途中で失敗すれば次の周期でやり直す(t *testing.T) {
	f := &fakeSys{mem: 16 * gib, cur: 4 * gib}
	c := newCtl(f)
	_ = c.Sync("8GB")
	c.Initial = func() (uint64, error) { return 0, errors.New("no meminfo") }
	if err := c.Sync(""); err == nil {
		t.Error("want error")
	}
	c.Initial = func() (uint64, error) { return 4 * gib, nil }
	if err := c.Sync(""); err != nil || f.cur != 4*gib {
		t.Errorf("retry: cur %d, %v", f.cur, err)
	}
}

func TestSync_ほかのmodprobe設定が再起動後に勝つことを知らせる(t *testing.T) {
	f := &fakeSys{mem: 16 * gib, cur: 4 * gib, conflict: "/etc/modprobe.d/zfs.conf"}
	err := newCtl(f).Sync("8GB")
	if err == nil || !strings.Contains(err.Error(), "/etc/modprobe.d/zfs.conf") {
		t.Errorf("err = %v", err)
	}
	if f.cur != 8*gib {
		t.Error("still applied now")
	}
}

func TestEffectiveMB_動いているカーネルの値をMBで返す(t *testing.T) {
	f := &fakeSys{cur: 3 * gib}
	if got := newCtl(f).EffectiveMB(); got != 3072 {
		t.Errorf("got %d", got)
	}
	f.curErr = errors.New("x")
	if got := newCtl(f).EffectiveMB(); got != 0 {
		t.Errorf("unreadable: %d", got)
	}
}

func TestCheck_保存前にこのノードで通るか確かめる(t *testing.T) {
	f := &fakeSys{mem: 16 * gib}
	c := newCtl(f)
	if err := c.Check("50%"); err != nil {
		t.Error(err)
	}
	if err := c.Check("1GB"); err != nil {
		t.Error(err)
	}
	if err := c.Check("99%"); err == nil {
		t.Error("too much memory: want error")
	}
	if err := c.Check("x"); err == nil {
		t.Error("bad syntax: want error")
	}
	f.memErr = errors.New("x")
	if err := c.Check("50%"); err != nil {
		t.Errorf("without memory info only the syntax is checked: %v", err)
	}
}

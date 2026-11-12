package sysmetrics_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/sysmetrics"
)

func write(t *testing.T, root, p, s string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
	if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSample_二回目から使用率と通信量を出す(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	s := &sysmetrics.Sampler{Root: root, Clock: func() time.Time { return now }}
	// user nice system idle iowait irq softirq steal guest guest_nice
	write(t, root, "proc/stat", "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3\n")
	write(t, root, "proc/meminfo", "MemTotal:       1000 kB\nMemFree: 1 kB\nMemAvailable:    250 kB\n")
	write(t, root, "proc/loadavg", "0.50 0.40 0.30 1/100 1\n")
	write(t, root, "proc/net/dev", "Inter-|\n face |\n    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n")
	write(t, root, "proc/spl/kstat/zfs/arcstats", "name type data\nhits 4 9\nsize 4 4096\n")
	first := s.Sample()
	if first.CPU != 0 || first.NetRx != 0 || first.MemTotal != 1024000 || first.MemUsed != 768000 || first.Load1 != 0.5 || first.ARC != 4096 {
		t.Errorf("first = %+v", first)
	}
	now = now.Add(10 * time.Second)
	write(t, root, "proc/stat", "cpu  150 0 150 750 150 0 0 0 80 0\n") // +100 busy of +200
	write(t, root, "proc/net/dev", "  eth0: 6000 0 0 0 0 0 0 0 4000 0 0 0 0 0 0 0\n    lo: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n")
	m := s.Sample()
	if math.Abs(m.CPU-50) > 0.01 || m.NetRx != 500 || m.NetTx != 200 {
		t.Errorf("second = %+v", m)
	}
	// Counters that went backwards (reboot, interface reset) give no rate.
	now = now.Add(10 * time.Second)
	write(t, root, "proc/net/dev", "  eth0: 10 0 0 0 0 0 0 0 10 0 0 0 0 0 0 0\n")
	if m := s.Sample(); m.NetRx != 0 || m.NetTx != 0 {
		t.Errorf("reset = %+v", m)
	}
}

func TestSample_読めないものは0にする(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/stat", "garbage\n")
	write(t, root, "proc/loadavg", "")
	write(t, root, "proc/meminfo", "MemTotal: 10 kB\nMemAvailable: 20 kB\n")
	write(t, root, "proc/net/dev", "  eth0: 1 2\n")
	s := &sysmetrics.Sampler{Root: root}
	_ = s.Sample()
	m := s.Sample()
	if m.CPU != 0 || m.Load1 != 0 || m.MemUsed != 0 || m.ARC != 0 || m.NetRx != 0 {
		t.Errorf("m = %+v", m)
	}
	if m := (&sysmetrics.Sampler{Root: t.TempDir()}).Sample(); m.MemTotal != 0 {
		t.Errorf("nothing: %+v", m)
	}
}

func TestHistory_新しいものだけ残す(t *testing.T) {
	h := sysmetrics.NewHistory(3)
	for i := 0; i < 5; i++ {
		h.Add(domain.Metrics{CPU: float64(i)})
	}
	got := h.List()
	if len(got) != 3 || got[0].CPU != 2 || got[2].CPU != 4 {
		t.Errorf("got %+v", got)
	}
	got[0].CPU = 99
	if h.List()[0].CPU != 2 {
		t.Error("List must copy")
	}
}

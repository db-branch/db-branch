// Package sysmetrics samples a Linux machine's CPU, memory, network and ZFS
// cache from /proc.
package sysmetrics

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Sampler turns successive /proc readings into rates. Root is "/" in
// production (tests point it elsewhere).
type Sampler struct {
	Root  string
	Clock func() time.Time

	mu       sync.Mutex
	prevAt   time.Time
	prevBusy uint64
	prevAll  uint64
	prevRx   uint64
	prevTx   uint64
}

func (s *Sampler) path(p string) string { return filepath.Join(s.Root, p) }

func (s *Sampler) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// Sample reads the machine now. Rates (CPU, network) need a previous sample:
// the first one reports them as 0.
func (s *Sampler) Sample() domain.Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m := domain.Metrics{Time: now.UTC().Truncate(time.Second)}
	busy, all := s.cpu()
	rx, tx := s.net()
	if !s.prevAt.IsZero() {
		if d := all - s.prevAll; all > s.prevAll {
			m.CPU = float64(busy-s.prevBusy) / float64(d) * 100
		}
		if secs := now.Sub(s.prevAt).Seconds(); secs > 0 {
			if rx >= s.prevRx {
				m.NetRx = float64(rx-s.prevRx) / secs
			}
			if tx >= s.prevTx {
				m.NetTx = float64(tx-s.prevTx) / secs
			}
		}
	}
	s.prevAt, s.prevBusy, s.prevAll, s.prevRx, s.prevTx = now, busy, all, rx, tx
	m.MemTotal, m.MemUsed = s.memory()
	m.Load1 = s.load()
	m.ARC = s.arc()
	return m
}

// cpu returns busy and total jiffies from the aggregate line of /proc/stat.
func (s *Sampler) cpu() (busy, all uint64) {
	b, err := os.ReadFile(s.path("proc/stat"))
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i == 7 || i == 8 { // guest time is already counted in user time
			continue
		}
		all += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, all
}

func (s *Sampler) memory() (total, used uint64) {
	f, err := os.Open(s.path("proc/meminfo"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	if avail > total {
		return total, 0
	}
	return total, total - avail
}

func (s *Sampler) load() float64 {
	b, err := os.ReadFile(s.path("proc/loadavg"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// net sums received and sent bytes over every interface but loopback.
func (s *Sampler) net() (rx, tx uint64) {
	b, err := os.ReadFile(s.path("proc/net/dev"))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx, tx = rx+r, tx+t
	}
	return rx, tx
}

func (s *Sampler) arc() uint64 {
	b, err := os.ReadFile(s.path("proc/spl/kstat/zfs/arcstats"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "size" {
			v, _ := strconv.ParseUint(f[2], 10, 64)
			return v
		}
	}
	return 0
}

// History keeps the latest samples (oldest first).
type History struct {
	mu   sync.Mutex
	size int
	buf  []domain.Metrics
}

// NewHistory keeps up to size samples.
func NewHistory(size int) *History { return &History{size: size} }

// Add appends a sample, dropping the oldest beyond the size.
func (h *History) Add(m domain.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf = append(h.buf, m)
	if len(h.buf) > h.size {
		h.buf = h.buf[len(h.buf)-h.size:]
	}
}

// List returns a copy of the samples.
func (h *History) List() []domain.Metrics {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]domain.Metrics{}, h.buf...)
}

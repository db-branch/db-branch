package domain_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

const gib = 1 << 30

func TestParseArcLimit_割合とサイズを読み空は引き継ぎ(t *testing.T) {
	tests := []struct {
		in   string
		mem  uint64
		want uint64
	}{
		{"50%", 32 * gib, 16 * gib},
		{" 12.5 % ", 16 * gib, 2 * gib},
		{"8GB", 32 * gib, 8 * gib},
		{"8gib", 32 * gib, 8 * gib},
		{"512MB", 32 * gib, 512 << 20},
		{"1.5g", 32 * gib, 3 * gib / 2},
		{"1048576KB", 32 * gib, gib},
		{"1TB", 4096 * gib, 1024 * gib},
	}
	for _, tt := range tests {
		l, err := domain.ParseArcLimit(tt.in)
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		got, err := l.Resolve(tt.mem)
		if err != nil || got != tt.want {
			t.Errorf("%q of %d: got %d, %v; want %d", tt.in, tt.mem, got, err, tt.want)
		}
	}
	l, err := domain.ParseArcLimit("")
	if err != nil || !l.Empty() {
		t.Errorf("empty: %+v, %v", l, err)
	}
}

func TestParseArcLimit_書式の誤りは分かる言葉で断る(t *testing.T) {
	for _, in := range []string{"abc", "8", "GB", "-1GB", "0%", "101%", "x%", "8XB", "0GB", "1e3GB"} {
		_, err := domain.ParseArcLimit(in)
		if err == nil || !strings.Contains(err.Error(), "50%") {
			t.Errorf("%q: err = %v, want a hint with an example", in, err)
		}
	}
}

func TestResolve_下限と物理メモリから予備を引いた上限を守る(t *testing.T) {
	mem := uint64(16 * gib) // reserve: the larger of 1 GiB and 10% = 1.6 GiB
	for _, in := range []string{"63MB", "0.1%"} {
		l, _ := domain.ParseArcLimit(in)
		if _, err := l.Resolve(mem); err == nil || !strings.Contains(err.Error(), "at least 64 MB") {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	for _, in := range []string{"16GB", "95%", "15GB"} {
		l, _ := domain.ParseArcLimit(in)
		if _, err := l.Resolve(mem); err == nil || !strings.Contains(err.Error(), "at most") {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	l, _ := domain.ParseArcLimit("64MB")
	if got, err := l.Resolve(mem); err != nil || got != 64<<20 {
		t.Errorf("lower bound: %d, %v", got, err)
	}
	// Small machine: the 1 GiB reserve wins over 10%.
	l, _ = domain.ParseArcLimit("90%")
	if _, err := l.Resolve(4 * gib); err == nil {
		t.Error("90% of 4 GiB leaves less than 1 GiB")
	}
	if _, err := (domain.ArcLimit{}).Resolve(mem); err == nil {
		t.Error("an empty limit has no value")
	}
}

func TestArcReserve_1GiBか1割の大きい方(t *testing.T) {
	if got := domain.ArcReserve(4 * gib); got != gib {
		t.Errorf("4 GiB: %d", got)
	}
	if got := domain.ArcReserve(64 * gib); got != 64*gib/10 {
		t.Errorf("64 GiB: %d", got)
	}
}

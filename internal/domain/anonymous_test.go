package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func intp(n int) *int { return &n }

func TestAnonymousLimits_未設定は継承で全体の上限だけを使う(t *testing.T) {
	var l domain.AnonymousLimits
	if !l.Inherits() || l.Disabled() {
		t.Errorf("zero value must inherit: %+v", l)
	}
	if got := l.EffectiveTTL(24 * time.Hour); got != 24*time.Hour {
		t.Errorf("ttl = %v", got)
	}
	if got := l.Remaining(10, 3, 3); got != 7 {
		t.Errorf("remaining = %d, want 7", got)
	}
}

func TestAnonymousLimits_ベース上限はクラスタ上限より厳しいほうが効く(t *testing.T) {
	cases := []struct {
		label                string
		max                  *int
		clusterMax, cl, base int
		want                 int
	}{
		{"base cap is the tighter one", intp(3), 10, 0, 1, 2},
		{"cluster cap is the tighter one", intp(8), 10, 7, 0, 3},
		{"base full", intp(2), 10, 5, 2, 0},
		{"over the cap is never negative", intp(2), 10, 5, 4, 0},
		{"cluster full", intp(5), 4, 4, 0, 0},
		{"zero disables the base", intp(0), 10, 0, 0, 0},
		{"inherit with a full cluster", nil, 10, 10, 0, 0},
	}
	for _, c := range cases {
		l := domain.AnonymousLimits{MaxBranches: c.max}
		if got := l.Remaining(c.clusterMax, c.cl, c.base); got != c.want {
			t.Errorf("%s: Remaining = %d, want %d", c.label, got, c.want)
		}
	}
}

func TestAnonymousLimits_ゼロは無効でありnilは無効ではない(t *testing.T) {
	if !(domain.AnonymousLimits{MaxBranches: intp(0)}).Disabled() {
		t.Error("0 must disable")
	}
	if (domain.AnonymousLimits{MaxBranches: intp(1)}).Disabled() || (domain.AnonymousLimits{}).Disabled() {
		t.Error("only 0 disables")
	}
}

func TestAnonymousLimits_TTLはクラスタ設定とベース設定の短いほう(t *testing.T) {
	cases := []struct {
		base, cluster, want time.Duration
	}{
		{0, 24 * time.Hour, 24 * time.Hour},
		{4 * time.Hour, 24 * time.Hour, 4 * time.Hour},
		{48 * time.Hour, 24 * time.Hour, 24 * time.Hour},
		{4 * time.Hour, 0, 4 * time.Hour},
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := (domain.AnonymousLimits{MaxTTL: c.base}).EffectiveTTL(c.cluster); got != c.want {
			t.Errorf("base %v cluster %v: %v, want %v", c.base, c.cluster, got, c.want)
		}
	}
}

func TestAnonymousLimits_負の値は受け付けない(t *testing.T) {
	for _, l := range []domain.AnonymousLimits{{MaxBranches: intp(-1)}, {MaxTTL: -time.Second}} {
		if err := l.Validate(); !errors.Is(err, domain.ErrInvalidAnonymous) {
			t.Errorf("%+v: %v", l, err)
		}
	}
	for _, l := range []domain.AnonymousLimits{{}, {MaxBranches: intp(0), MaxTTL: time.Hour}} {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
}

func TestAnonymousLimits_コピーはポインタを共有しない(t *testing.T) {
	l := domain.AnonymousLimits{MaxBranches: intp(2), MaxTTL: time.Hour}
	c := l.Clone()
	*c.MaxBranches = 9
	if *l.MaxBranches != 2 {
		t.Error("Clone shares MaxBranches")
	}
	if (domain.AnonymousLimits{}).Clone().MaxBranches != nil {
		t.Error("inherit stays inherit")
	}
}

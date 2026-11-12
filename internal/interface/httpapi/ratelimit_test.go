package httpapi_test

import (
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/interface/httpapi"
)

func limiterAt(now *time.Time) *httpapi.IPLimiter {
	return &httpapi.IPLimiter{Now: func() time.Time { return *now }}
}

func TestIPLimiter_窓の中では上限までで超えたら待ち時間を返す(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("1.2.3.4", 3, time.Hour); !ok {
			t.Fatalf("call %d refused", i+1)
		}
		now = now.Add(10 * time.Minute)
	}
	ok, retry := l.Allow("1.2.3.4", 3, time.Hour)
	if ok {
		t.Fatal("the 4th call within the hour must be refused")
	}
	// The first call was 30 minutes ago, so it leaves the window in 30 more.
	if retry != 30*time.Minute {
		t.Errorf("retry = %v, want 30m", retry)
	}
	if ok, _ := l.Allow("1.2.3.4", 3, time.Hour); ok {
		t.Error("a refused call must not use up the window")
	}
}

func TestIPLimiter_窓がずれると古い分から空く(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	l.Allow("a", 2, time.Hour)
	now = now.Add(40 * time.Minute)
	l.Allow("a", 2, time.Hour)
	now = now.Add(21 * time.Minute) // the first is 61 minutes old
	if ok, _ := l.Allow("a", 2, time.Hour); !ok {
		t.Fatal("the oldest call left the window")
	}
	if ok, retry := l.Allow("a", 2, time.Hour); ok || retry != 39*time.Minute {
		t.Errorf("second call: ok=%v retry=%v, want refused for 39m", ok, retry)
	}
}

func TestIPLimiter_クライアントごとに数える(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	l.Allow("a", 1, time.Hour)
	if ok, _ := l.Allow("a", 1, time.Hour); ok {
		t.Error("a is over")
	}
	if ok, _ := l.Allow("b", 1, time.Hour); !ok {
		t.Error("b must not share a's count")
	}
}

func TestIPLimiter_ゼロ以下は無制限(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow("a", 0, time.Hour); !ok {
			t.Fatal("0 means unlimited")
		}
	}
	if ok, _ := l.Allow("a", -1, time.Hour); !ok {
		t.Error("negative means unlimited too")
	}
	if l.Len() != 0 {
		t.Errorf("unlimited must remember nothing, has %d", l.Len())
	}
}

func TestIPLimiter_返した分は数えない(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	l.Allow("a", 1, time.Hour)
	l.Refund("a")
	if ok, _ := l.Allow("a", 1, time.Hour); !ok {
		t.Error("a refunded call must free its place")
	}
	l.Refund("nobody") // no record: nothing happens
	l.Refund("a")
	l.Refund("a")
	if l.Len() != 0 {
		t.Errorf("an emptied client must be forgotten, has %d", l.Len())
	}
}

func TestIPLimiter_古い記録は忘れてメモリを使い続けない(t *testing.T) {
	now := t0
	l := limiterAt(&now)
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k, 5, time.Hour)
	}
	if l.Len() != 3 {
		t.Fatalf("len = %d", l.Len())
	}
	now = now.Add(2 * time.Hour)
	l.Allow("d", 5, time.Hour)
	if l.Len() != 1 {
		t.Errorf("after the window only d remains, len = %d", l.Len())
	}
}

func TestClientKey_ポート抜きのアドレスでIPv6は前半64ビットにまとめる(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7:5555":           "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:1": "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb::9]:2": "2001:db8:1:2::/64",
		"[::ffff:192.0.2.7]:80":    "192.0.2.7",
		"@":                        "@",
		"garbage":                  "garbage",
	}
	for in, want := range cases {
		if got := httpapi.ClientKey(in); got != want {
			t.Errorf("ClientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
)

func intPtr(n int) *int { return &n }

// anonFixture has two bases with a public snapshot (prod, demo), cluster-wide
// limits of 2h and 5 branches, and no rate limit.
func anonFixture(t *testing.T) *fixture {
	t.Helper()
	old := httpapi.PublicSettle
	t.Cleanup(func() { httpapi.PublicSettle = old })
	httpapi.PublicSettle = 20 * time.Millisecond
	f := ownedFixture(t)
	for _, n := range []string{"prod", "demo"} {
		f.branches.bases[n] = domain.Base{Name: name(n), Runtime: mysql84, Status: domain.BaseReady,
			Snapshots: []domain.BaseSnapshot{{Name: "s1", CreatedAt: t0, Public: true}}}
	}
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 5 }
	f.srv.AnonymousRate = func() int { return 0 }
	return f
}

func (f *fixture) seedAnonymous(base string, names ...string) {
	for _, n := range names {
		f.branches.branches[n] = domain.Branch{Name: name(n), Base: name(base), Status: domain.BranchReady, Owner: "anonymous", Access: domain.AccessOpen, Runtime: mysql84}
	}
}

func (f *fixture) setAnonymous(base string, l domain.AnonymousLimits) {
	b := f.branches.bases[base]
	b.Anonymous = l
	f.branches.bases[base] = b
}

func anonCreate(t *testing.T, h http.Handler, n, base string, mutate ...func(*http.Request)) (*http.Response, map[string]any) {
	t.Helper()
	var m func(*http.Request)
	if len(mutate) > 0 {
		m = mutate[0]
	}
	return send(t, h, "POST", "/v1/branches", map[string]any{"name": n, "base": base}, m)
}

func TestAnonymous_ベースごとの上限はそのベースのブランチ数だけを数える(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(2)})
	f.seedAnonymous("prod", "p1", "p2")
	resp, body := anonCreate(t, h, "p3", "prod")
	if resp.StatusCode != 409 {
		t.Fatalf("over the base cap: %d %v", resp.StatusCode, body)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "2 branches") || !strings.Contains(msg, "base prod") || !strings.Contains(msg, "most it allows") {
		t.Errorf("the message must say the numbers and the base: %q", msg)
	}
	// Other bases are not affected by prod being full.
	if resp, body := anonCreate(t, h, "d1", "demo"); resp.StatusCode != 202 {
		t.Errorf("demo inherits: %d %v", resp.StatusCode, body)
	}
}

func TestAnonymous_全体の上限はベースが継承でも超えられない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(9)}) // looser than the cluster's 5
	f.seedAnonymous("demo", "d1", "d2", "d3", "d4", "d5")
	resp, body := anonCreate(t, h, "p1", "prod")
	if resp.StatusCode != 409 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "5 branches") {
		t.Errorf("cluster cap: %d %v", resp.StatusCode, body)
	}
}

func TestAnonymous_ベースの上限が0なら公開スナップショットでも作れない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(0)})
	resp, body := anonCreate(t, h, "p1", "prod")
	if resp.StatusCode != 403 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "base prod") {
		t.Errorf("disabled: %d %v", resp.StatusCode, body)
	}
	// The same caller with a login is not affected.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "p2", "base": "prod"}, as("t-alice")); resp.StatusCode != 202 {
		t.Errorf("signed in: %d", resp.StatusCode)
	}
}

func TestAnonymous_継承なら全体の上限だけが効く(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.seedAnonymous("prod", "p1", "p2", "p3", "p4")
	if resp, body := anonCreate(t, h, "p5", "prod"); resp.StatusCode != 202 {
		t.Errorf("4 of 5: %d %v", resp.StatusCode, body)
	}
}

func TestAnonymous_TTLは全体とベースの短いほうに切り詰める(t *testing.T) {
	cases := []struct {
		label string
		base  time.Duration
		ask   string
		want  time.Duration
	}{
		{"inherit", 0, "100h", 2 * time.Hour},
		{"base shorter", 30 * time.Minute, "100h", 30 * time.Minute},
		{"base longer than the cluster", 9 * time.Hour, "100h", 2 * time.Hour},
		{"shorter asked than both", 30 * time.Minute, "10m", 10 * time.Minute},
		{"none asked", 45 * time.Minute, "", 45 * time.Minute},
	}
	for i, c := range cases {
		f := anonFixture(t)
		h := f.srv.Handler(false)
		f.setAnonymous("prod", domain.AnonymousLimits{MaxTTL: c.base})
		n := "ttl" + string(rune('a'+i))
		body := map[string]any{"name": n, "base": "prod"}
		if c.ask != "" {
			body["ttl"] = c.ask
		}
		if resp, out := send(t, h, "POST", "/v1/branches", body, nil); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", c.label, resp.StatusCode, out)
		}
		waitFor(t, func() bool { _, ok := f.branches.get(n); return ok })
		got, _ := f.branches.get(n)
		if got.ExpiresAt == nil || got.ExpiresAt.Sub(t0) != c.want {
			t.Errorf("%s: expires %v, want +%v", c.label, got.ExpiresAt, c.want)
		}
	}
}

func TestAnonymous_公開でないスナップショットは上限の設定があっても作れない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	b := f.branches.bases["prod"]
	b.Snapshots = []domain.BaseSnapshot{{Name: "s1", CreatedAt: t0}}
	b.Anonymous = domain.AnonymousLimits{MaxBranches: intPtr(100), MaxTTL: time.Hour}
	f.branches.bases["prod"] = b
	if resp, body := anonCreate(t, h, "p1", "prod"); resp.StatusCode != 403 {
		t.Errorf("private snapshot: %d %v", resp.StatusCode, body)
	}
}

func TestAnonymous_IPごとの回数制限は超えると429とRetry_Afterを返す(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.srv.AnonymousRate = func() int { return 2 }
	from := func(addr string) func(*http.Request) { return func(r *http.Request) { r.RemoteAddr = addr } }
	for _, n := range []string{"r1", "r2"} {
		if resp, body := anonCreate(t, h, n, "prod", from("203.0.113.5:4000")); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", n, resp.StatusCode, body)
		}
	}
	resp, body := anonCreate(t, h, "r3", "prod", from("203.0.113.5:4001")) // another port, same client
	if resp.StatusCode != 429 {
		t.Fatalf("3rd from one client: %d %v", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q", ra)
	}
	e := body["error"].(map[string]any)
	if e["code"] != "rate_limited" || !strings.Contains(e["message"].(string), "2 branches per hour") {
		t.Errorf("error = %v", e)
	}
	// Another client has its own allowance.
	if resp, _ := anonCreate(t, h, "r4", "prod", from("203.0.113.6:4000")); resp.StatusCode != 202 {
		t.Errorf("other client: %d", resp.StatusCode)
	}
	// A proxy header does not change who the client is.
	spoof := func(r *http.Request) {
		r.RemoteAddr = "203.0.113.5:4002"
		r.Header.Set("X-Forwarded-For", "198.51.100.1")
	}
	if resp, _ := anonCreate(t, h, "r5", "prod", spoof); resp.StatusCode != 429 {
		t.Errorf("X-Forwarded-For must be ignored: %d", resp.StatusCode)
	}
	// People with a login are not limited by it.
	for _, n := range []string{"u1", "u2", "u3"} {
		m := func(r *http.Request) { r.RemoteAddr = "203.0.113.5:4003"; as("t-alice")(r) }
		if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": n, "base": "prod"}, m); resp.StatusCode != 202 {
			t.Errorf("%s with a token: %d", n, resp.StatusCode)
		}
	}
}

func TestAnonymous_回数制限0は無制限(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	for _, n := range []string{"u1", "u2", "u3", "u4"} {
		if resp, body := anonCreate(t, h, n, "prod"); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", n, resp.StatusCode, body)
		}
	}
}

func TestAnonymous_回数制限は未配線なら既定の5回(t *testing.T) {
	f := anonFixture(t)
	f.srv.AnonymousRate = nil
	h := f.srv.Handler(false)
	for _, n := range []string{"u1", "u2", "u3", "u4", "u5"} {
		if resp, body := anonCreate(t, h, n, "prod"); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", n, resp.StatusCode, body)
		}
	}
	if resp, _ := anonCreate(t, h, "u6", "prod"); resp.StatusCode != 429 {
		t.Errorf("6th: %d", resp.StatusCode)
	}
}

func TestAnonymous_断られた作成は回数を消費しない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.srv.AnonymousRate = func() int { return 1 }
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(0)})
	for i := 0; i < 3; i++ {
		if resp, _ := anonCreate(t, h, "x", "prod"); resp.StatusCode != 403 {
			t.Fatalf("refused create %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := anonCreate(t, h, "x", "demo"); resp.StatusCode != 202 {
		t.Errorf("the allowance must be intact: %d", resp.StatusCode)
	}
}

func TestAnonymous_既存の名前や配置の失敗でも回数を返す(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.srv.AnonymousRate = func() int { return 1 }
	f.seedAnonymous("demo", "taken")
	if resp, _ := anonCreate(t, h, "taken", "prod"); resp.StatusCode != 409 {
		t.Fatalf("name taken: %d", resp.StatusCode)
	}
	f.branches.placeErr = domain.ErrNoCapacity
	if resp, _ := anonCreate(t, h, "y", "prod"); resp.StatusCode != 503 {
		t.Fatalf("no capacity: %d", resp.StatusCode)
	}
	f.branches.placeErr = nil
	if resp, body := anonCreate(t, h, "z", "prod"); resp.StatusCode != 202 {
		t.Errorf("the allowance must be intact: %d %v", resp.StatusCode, body)
	}
}

func TestPublicBases_ベースごとの実効上限と残りを返す(t *testing.T) {
	f := anonFixture(t)
	f.srv.AnonymousRate = func() int { return 7 }
	f.branches.bases["off"] = domain.Base{Name: name("off"), Runtime: mysql84, Status: domain.BaseReady,
		Snapshots: []domain.BaseSnapshot{{Name: "o1", CreatedAt: t0, Public: true}}, Anonymous: domain.AnonymousLimits{MaxBranches: intPtr(0)}}
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(3), MaxTTL: 30 * time.Minute})
	f.setAnonymous("demo", domain.AnonymousLimits{MaxBranches: intPtr(9), MaxTTL: 9 * time.Hour})
	f.seedAnonymous("prod", "p1")
	f.seedAnonymous("demo", "d1", "d2", "d3")
	f.seedAnonymous("other", "o1")
	resp, body := send(t, f.srv.Handler(false), "GET", "/v1/public/bases", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	got := map[string]map[string]any{}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		got[m["name"].(string)] = m["anonymous"].(map[string]any)
	}
	if _, ok := got["off"]; ok || len(got) != 2 {
		t.Errorf("a base with 0 must not be offered: %v", got)
	}
	// prod: own cap 3, 1 used -> 2; cluster has 5 with 5 used in total -> 0.
	if p := got["prod"]; p["max_branches"] != 3.0 || p["max_ttl"] != "30m0s" || p["branches"] != 1.0 || p["remaining"] != 0.0 {
		t.Errorf("prod = %v", p)
	}
	// demo: the cluster cap (5) is tighter than its own 9; the TTL likewise (2h).
	if d := got["demo"]; d["max_branches"] != 5.0 || d["max_ttl"] != "2h0m0s" || d["branches"] != 3.0 || d["remaining"] != 0.0 {
		t.Errorf("demo = %v", d)
	}
	top := body["anonymous"].(map[string]any)
	if top["max_ttl"] != "2h0m0s" || top["max_branches"] != 5.0 || top["branches"] != 5.0 || top["rate_per_hour"] != 7.0 {
		t.Errorf("cluster-wide = %v", top)
	}
}

func TestPublicBases_空きがあれば残りを数える(t *testing.T) {
	f := anonFixture(t)
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(3)})
	f.seedAnonymous("prod", "p1")
	_, body := send(t, f.srv.Handler(false), "GET", "/v1/public/bases", nil, nil)
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		want := map[string]float64{"prod": 2, "demo": 4}[m["name"].(string)]
		if r := m["anonymous"].(map[string]any)["remaining"]; r != want {
			t.Errorf("%v remaining = %v, want %v", m["name"], r, want)
		}
	}
}

func TestBases_匿名の上限をGETで見せPATCHで部分更新して継承に戻せる(t *testing.T) {
	f := anonFixture(t)
	_, body := f.do(t, "GET", "/v1/bases/prod", nil)
	a := body["anonymous"].(map[string]any)
	if v, ok := a["max_branches"]; !ok || v != nil {
		t.Errorf("inherit must read as an explicit null: %v", a)
	}
	if v, ok := a["max_ttl"]; !ok || v != nil {
		t.Errorf("inherit must read as an explicit null: %v", a)
	}
	resp, body := f.do(t, "PATCH", "/v1/bases/prod/anonymous", map[string]any{"max_branches": 3, "max_ttl": "4h"})
	if resp.StatusCode != 200 {
		t.Fatalf("set: %d %v", resp.StatusCode, body)
	}
	if a = body["anonymous"].(map[string]any); a["max_branches"] != 3.0 || a["max_ttl"] != "4h0m0s" {
		t.Errorf("set = %v", a)
	}
	// Absent keys stay as they are.
	_, body = f.do(t, "PATCH", "/v1/bases/prod/anonymous", map[string]any{"max_branches": 0})
	if a = body["anonymous"].(map[string]any); a["max_branches"] != 0.0 || a["max_ttl"] != "4h0m0s" {
		t.Errorf("partial = %v", a)
	}
	// The list shows it too.
	_, list := f.do(t, "GET", "/v1/bases", nil)
	for _, it := range list["items"].([]any) {
		if m := it.(map[string]any); m["name"] == "prod" && m["anonymous"].(map[string]any)["max_branches"] != 0.0 {
			t.Errorf("list = %v", m["anonymous"])
		}
	}
	// null inherits.
	_, body = f.do(t, "PATCH", "/v1/bases/prod/anonymous", map[string]any{"max_branches": nil, "max_ttl": nil})
	if a = body["anonymous"].(map[string]any); a["max_branches"] != nil || a["max_ttl"] != nil {
		t.Errorf("inherit = %v", a)
	}
	if got := f.branches.bases["prod"].Anonymous; !got.Inherits() {
		t.Errorf("stored %+v", got)
	}
}

func TestBases_匿名の上限の不正な指定は400(t *testing.T) {
	f := anonFixture(t)
	for _, c := range []map[string]any{
		{},
		{"max_branches": -1},
		{"max_branches": "many"},
		{"max_branches": 1.5},
		{"max_ttl": "soon"},
		{"max_ttl": "-1h"},
		{"max_ttl": "0s"},
		{"max_ttl": 5},
	} {
		if resp, body := f.do(t, "PATCH", "/v1/bases/prod/anonymous", c); resp.StatusCode != 400 {
			t.Errorf("%v: %d %v", c, resp.StatusCode, body)
		}
	}
	if resp, _ := f.do(t, "PATCH", "/v1/bases/nope/anonymous", map[string]any{"max_branches": 1}); resp.StatusCode != 404 {
		t.Errorf("missing: %d", resp.StatusCode)
	}
	f.branches.fail = domain.ErrConflict
	if resp, _ := f.do(t, "PATCH", "/v1/bases/prod/anonymous", map[string]any{"max_branches": 1}); resp.StatusCode != 409 {
		t.Errorf("service error: %d", resp.StatusCode)
	}
}

func TestBases_匿名の上限を変えられるのは管理者だけ(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	body := map[string]any{"max_branches": 1}
	if resp, _ := send(t, h, "PATCH", "/v1/bases/prod/anonymous", body, nil); resp.StatusCode != 401 {
		t.Errorf("no login: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "PATCH", "/v1/bases/prod/anonymous", body, as("t-alice")); resp.StatusCode != 403 {
		t.Errorf("branches-scoped token: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "PATCH", "/v1/bases/prod/anonymous", body, as("t-full")); resp.StatusCode != 200 {
		t.Errorf("full token: %d", resp.StatusCode)
	}
}

func TestAnonymous_ノード間の転送は元のノードで数えたので再び数えない(t *testing.T) {
	f := anonFixture(t)
	f.srv.AnonymousRate = func() int { return 1 }
	forwarded := func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "node2")
		r.Header.Set("X-Db-Branch-Principal", `{"ID":"anonymous","Name":"anonymous","Roles":["anonymous"]}`)
	}
	local := f.srv.Handler(true)
	for _, n := range []string{"f1", "f2", "f3"} {
		if resp, body := anonCreate(t, local, n, "prod", forwarded); resp.StatusCode != 202 {
			t.Fatalf("%s forwarded: %d %v", n, resp.StatusCode, body)
		}
	}
}

func TestAnonymous_転送ヘッダを自分で付けても回数制限は外せない(t *testing.T) {
	f := anonFixture(t)
	f.srv.AnonymousRate = func() int { return 1 }
	h := f.srv.Handler(false)
	spoof := func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "node2")
		r.Header.Set("X-Db-Branch-Principal", `{"ID":"anonymous","Name":"anonymous","Roles":["anonymous"]}`)
	}
	if resp, _ := anonCreate(t, h, "s1", "prod", spoof); resp.StatusCode != 202 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	if resp, _ := anonCreate(t, h, "s2", "prod", spoof); resp.StatusCode != 429 {
		t.Errorf("a forged forwarding header must not skip the limit: %d", resp.StatusCode)
	}
}

func TestPublicBases_ブランチ一覧が読めなければ失敗を返す(t *testing.T) {
	f := anonFixture(t)
	f.branches.fail = errors.New("store down")
	if resp, _ := send(t, f.srv.Handler(false), "GET", "/v1/public/bases", nil, nil); resp.StatusCode != 500 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestAnonymous_作成中のブランチも上限に数え並行作成で超過しない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	f.branches.createGate = make(chan struct{})
	t.Cleanup(func() { close(f.branches.createGate) })
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 2 }
	for _, n := range []string{"c1", "c2"} {
		if resp, body := anonCreate(t, h, n, "prod"); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", n, resp.StatusCode, body)
		}
	}
	// c1 and c2 are still being made (not in the list): c3 is over the cap.
	if resp, body := anonCreate(t, h, "c3", "prod"); resp.StatusCode != 409 {
		t.Errorf("3rd while two are pending: %d %v", resp.StatusCode, body)
	}
	// The per-base cap counts them as well.
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 10 }
	f.setAnonymous("prod", domain.AnonymousLimits{MaxBranches: intPtr(2)})
	if resp, body := anonCreate(t, h, "c4", "prod"); resp.StatusCode != 409 {
		t.Errorf("per-base cap while pending: %d %v", resp.StatusCode, body)
	}
}

func TestAnonymous_転送先が202以外を返したらレート枠を返金する(t *testing.T) {
	f := anonFixture(t)
	c := &fakeCluster{replies: map[string]int{"n2": http.StatusConflict}}
	f.srv.Cluster = c
	f.branches.place = "n2"
	f.srv.AnonymousRate = func() int { return 1 }
	h := f.srv.Handler(false)
	for _, n := range []string{"f1", "f2"} {
		if resp, body := anonCreate(t, h, n, "prod"); resp.StatusCode != 409 {
			t.Fatalf("%s: the other node refuses with 409, got %d %v", n, resp.StatusCode, body)
		}
	}
}

func TestAnonymous_他ノードへ転送中のブランチも転送元の上限に数える(t *testing.T) {
	f := anonFixture(t)
	f.srv.Cluster = &fakeCluster{replies: map[string]int{"n2": http.StatusAccepted}}
	f.branches.place = "n2"
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 1 }
	h := f.srv.Handler(false)
	if resp, body := anonCreate(t, h, "m1", "prod"); resp.StatusCode != 202 {
		t.Fatalf("m1: %d %v", resp.StatusCode, body)
	}
	if resp, body := anonCreate(t, h, "m2", "prod"); resp.StatusCode != 409 {
		t.Errorf("m1 is being made on n2, m2 is over the cap: %d %v", resp.StatusCode, body)
	}
}

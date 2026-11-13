package licensing_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

func TestRenewalDue_期限が近いか過ぎたサブスクリプションのキーだけ更新する(t *testing.T) {
	day := 24 * time.Hour
	base := license.License{Subscription: "sub_1", RenewURL: "https://x.example/renew"}
	cases := []struct {
		name    string
		billing string
		left    time.Duration
		want    bool
	}{
		{"月額 残り6日", license.Monthly, 6 * day, true},
		{"月額 残り8日", license.Monthly, 8 * day, false},
		{"月額 期限ちょうど7日前は未だ", license.Monthly, 7 * day, false},
		{"年額 残り29日", license.Yearly, 29 * day, true},
		{"年額 残り31日", license.Yearly, 31 * day, false},
		{"年額 残り8日", license.Yearly, 8 * day, true},
		{"請求の種類が空でも猶予を長めに取る", "", 20 * day, true},
		{"猶予期間に入った", license.Monthly, -3 * day, true},
		{"猶予も過ぎた", license.Monthly, -90 * day, true},
	}
	for _, c := range cases {
		l := base
		l.Billing, l.Expires = c.billing, t0.Add(c.left)
		if got := licensing.RenewalDue(l, t0); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	for name, l := range map[string]license.License{
		"サブスクリプションなし": {RenewURL: "https://x.example", Expires: t0},
		"更新URLなし":     {Subscription: "s", Expires: t0},
	} {
		if licensing.RenewalDue(l, t0.Add(time.Hour)) {
			t.Errorf("%s は更新しない", name)
		}
	}
}

func TestCheckRenewURL_httpsだけを許しテスト用にローカルのhttpを許す(t *testing.T) {
	for _, ok := range []string{"https://licenses.example.com/v1/renew", "http://127.0.0.1:8080/renew", "http://localhost/renew"} {
		if err := licensing.CheckRenewURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://licenses.example.com/renew", "ftp://x/y", "https://", "://", "licenses.example.com", "http://127.0.0.1.example.com/"} {
		if err := licensing.CheckRenewURL(bad); !errors.Is(err, licensing.ErrInsecureURL) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestAccept_同じライセンスで期限が延び有効なモジュールを含む鍵だけ受け入れる(t *testing.T) {
	cur := license.License{ID: "L1", Customer: "Acme", Modules: []string{"auth", "audit"}, IssuedAt: t0, Expires: t0}
	next := func(f func(*license.License)) license.License {
		n := cur
		n.Modules = []string{"auth", "audit", "sso"}
		n.Expires = t0.AddDate(0, 1, 0)
		f(&n)
		return n
	}
	wanted := []string{"audit", "proxysql"}
	cases := []struct {
		name string
		next license.License
		want string
	}{
		{"正しい更新", next(func(*license.License) {}), ""},
		{"別のライセンスID", next(func(l *license.License) { l.ID = "L2" }), "licence"},
		{"別の顧客", next(func(l *license.License) { l.Customer = "Evil" }), "customer"},
		{"期限が前", next(func(l *license.License) { l.Expires = t0.Add(-time.Hour) }), "not later"},
		{"期限が同じ", next(func(l *license.License) { l.Expires = t0 }), "not later"},
		{"期限が前でも後から発行されたプラン変更は受け入れる", next(func(l *license.License) {
			l.Expires = t0.Add(-time.Hour)
			l.IssuedAt = t0.Add(time.Minute)
			l.Plan = license.Business
		}), ""},
		{"期限が前で発行も前なら断る", next(func(l *license.License) { l.Expires = t0.Add(-time.Hour); l.IssuedAt = t0.Add(-time.Minute) }), "not issued later"},
		{"後から発行されても別の顧客は断る", next(func(l *license.License) { l.IssuedAt = t0.Add(time.Minute); l.Customer = "X" }), "customer"},
		{"後から発行されてもモジュールが減るなら断る", next(func(l *license.License) {
			l.Expires = t0
			l.IssuedAt = t0.Add(time.Minute)
			l.Modules = []string{"auth"}
		}), "audit"},
		{"有効なモジュールが外れる", next(func(l *license.License) { l.Modules = []string{"auth"} }), "audit"},
		{"無効なモジュールが外れても良い", next(func(l *license.License) { l.Modules = []string{"audit"} }), ""},
		{"鍵に無いモジュールは数えない", next(func(l *license.License) { l.Modules = []string{"audit", "auth"} }), ""},
	}
	for _, c := range cases {
		err := licensing.Accept(cur, c.next, wanted)
		switch {
		case c.want == "" && err != nil, c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v", c.name, err)
		}
	}
	star := cur
	star.Modules = []string{"*"}
	n := next(func(l *license.License) { l.Modules = []string{"*"} })
	if err := licensing.Accept(star, n, wanted); err != nil {
		t.Errorf("* から *: %v", err)
	}
	if err := licensing.Accept(star, next(func(*license.License) {}), wanted); err == nil {
		t.Error("* から個別のモジュールへは変えない")
	}
}

type fakeClient struct {
	mu    sync.Mutex
	calls []string // "url key"
	// clusters is the cluster id of each call ("" when none was sent).
	clusters []string
	key      string
	err      error
	block    chan struct{}
}

func (f *fakeClient) Renew(_ context.Context, u, key, clusterID string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, u+" "+key)
	f.clusters = append(f.clusters, clusterID)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.key, f.err
}

func (f *fakeClient) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type mapSettings struct {
	mu      sync.Mutex
	v       map[string]string
	putFail error
}

func (m *mapSettings) Values(context.Context, string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.v {
		out[k] = v
	}
	return out, nil
}

func (m *mapSettings) Put(_ context.Context, _ string, v map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putFail != nil {
		return m.putFail
	}
	for k, x := range v {
		m.v[k] = x
	}
	return nil
}

type notice struct{ event, level, subject, text string }

type rig struct {
	r      *licensing.Renewer
	root   license.LocalSigner
	client *fakeClient
	set    *mapSettings
	mods   *fakeModules
	now    time.Time
	leader bool
	told   []notice
}

func (g *rig) issue(t *testing.T, l license.License) string {
	t.Helper()
	issuing := license.NewLocalSigner()
	cert, err := license.Certify(ctx, g.root, issuing.Public(), "t", t0.AddDate(-1, 0, 0), t0.AddDate(3, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	key, err := license.Issue(ctx, issuing, cert, l)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func subLicense(expires time.Time) license.License {
	return license.License{ID: "L1", Customer: "Acme", Modules: []string{"auth", "audit"}, MaxNodes: 5, IssuedAt: t0.AddDate(0, -1, 0), Expires: expires,
		GraceDays: 7, Plan: license.Team, Billing: license.Monthly, Subscription: "sub_1", RenewURL: "https://licenses.example.com/renew"}
}

func newRig(t *testing.T, cur license.License) *rig {
	root := license.NewLocalSigner()
	g := &rig{root: root, client: &fakeClient{}, now: t0, leader: true}
	g.mods = &fakeModules{available: map[string]string{"auth": "", "audit": ""}, specs: []domain.ModuleSpec{{Name: "audit", Wanted: true}}}
	g.set = &mapSettings{v: map[string]string{licensing.KeySetting: g.issue(t, cur)}}
	svc := &licensing.Service{Root: root.Public(), Clock: func() time.Time { return g.now }, Nodes: func() int { return 3 }, Modules: g.mods, Settings: g.set}
	g.r = &licensing.Renewer{Service: svc, Client: g.client, Leader: func() bool { return g.leader }, Jitter: func() time.Duration { return 0 },
		Notify: func(_ context.Context, e, l, s, x string) { g.told = append(g.told, notice{e, l, s, x}) }}
	return g
}

func (g *rig) savedKey() string { return g.set.v[licensing.KeySetting] }

// 月額のキー（期限まで3日）を持つクラスタ。
func dueRig(t *testing.T) *rig { return newRig(t, subLicense(t0.AddDate(0, 0, 3))) }

func TestRenewer_期限が近い鍵は更新された鍵に差し替え現在の鍵だけを送る(t *testing.T) {
	g := dueRig(t)
	old := g.savedKey()
	g.client.key = g.issue(t, func() license.License {
		l := subLicense(t0.AddDate(0, 1, 3))
		l.Modules = []string{"auth", "audit", "sso"}
		return l
	}())
	st, err := g.r.CheckNow(ctx)
	if err != nil || st.LastResult != licensing.ResultRenewed || st.LastAttempt == nil || st.NextAttempt == nil {
		t.Fatalf("%+v %v", st, err)
	}
	if g.savedKey() != g.client.key {
		t.Error("更新された鍵が保存されていない")
	}
	if len(g.client.calls) != 1 || g.client.calls[0] != "https://licenses.example.com/renew "+old {
		t.Errorf("送ったもの: %v", g.client.calls)
	}
	if len(g.mods.installed) != 0 {
		t.Errorf("更新ではモジュールを有効にしない: %v", g.mods.installed)
	}
	if want := t0.Add(licensing.CheckEvery); !st.NextAttempt.Equal(want) {
		t.Errorf("次は12時間後: %v", st.NextAttempt)
	}
	if !strings.Contains(st.Message, "valid until") || len(g.told) != 0 {
		t.Errorf("%q %v", st.Message, g.told)
	}
}

// memClusterID is a ClusterIDStore in memory.
type memClusterID struct {
	id      string
	initErr error
	inits   int
}

func (m *memClusterID) ClusterID(context.Context) string { return m.id }

func (m *memClusterID) InitClusterID(_ context.Context, id string) (string, error) {
	m.inits++
	if m.initErr != nil {
		return "", m.initErr
	}
	if m.id == "" {
		m.id = id
	}
	return m.id, nil
}

func TestRenewer_クラスタidがまだなければ乱数で作って記録し毎回同じidを送る(t *testing.T) {
	g := dueRig(t)
	store := &memClusterID{}
	g.r.Cluster = store
	g.r.Random = bytes.NewReader(append(bytes.Repeat([]byte{0x0a}, 16), bytes.Repeat([]byte{0x0b}, 16)...))
	g.client.err = licensing.ErrNoNewerKey
	for i := 0; i < 2; i++ {
		g.now = g.now.Add(time.Minute)
		if _, err := g.r.CheckNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := strings.Repeat("0a", 16)
	if store.id != want || len(g.client.clusters) != 2 || g.client.clusters[0] != want || g.client.clusters[1] != want {
		t.Errorf("記録 %q 送ったid %q", store.id, g.client.clusters)
	}
}

func TestRenewer_記録済みのクラスタidはそのまま送り作り直さない(t *testing.T) {
	g := dueRig(t)
	existing := strings.Repeat("c3", 16)
	store := &memClusterID{id: existing}
	g.r.Cluster = store
	g.r.Random = errReader{}
	g.client.err = licensing.ErrNoNewerKey
	if _, err := g.r.CheckNow(ctx); err != nil {
		t.Fatal(err)
	}
	if store.inits != 0 || len(g.client.clusters) != 1 || g.client.clusters[0] != existing {
		t.Errorf("記録の試み %d 送ったid %q", store.inits, g.client.clusters)
	}
}

func TestRenewer_クラスタidを用意できなくてもidなしで更新する(t *testing.T) {
	cases := []struct {
		name   string
		store  *memClusterID
		random io.Reader
	}{
		{"クラスタが記録を受け付けない", &memClusterID{initErr: domain.ErrClusterUpgrading}, nil},
		{"乱数が読めない", &memClusterID{}, errReader{}},
		{"保管先がない", nil, nil},
	}
	for _, tc := range cases {
		g := dueRig(t)
		if tc.store != nil {
			g.r.Cluster = tc.store
		}
		g.r.Random = tc.random
		g.client.err = licensing.ErrNoNewerKey
		st, err := g.r.CheckNow(ctx)
		if err != nil || st.LastResult != licensing.ResultNotYet || len(g.client.clusters) != 1 || g.client.clusters[0] != "" {
			t.Errorf("%s: %+v %v 送ったid %q", tc.name, st, err, g.client.clusters)
		}
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestRenewer_期限まで余裕があれば通信しない(t *testing.T) {
	g := newRig(t, subLicense(t0.AddDate(0, 0, 20)))
	g.leader = true
	g.r.Tick(ctx) // リーダーになった
	g.now = t0.Add(licensing.FirstCheckAfter)
	g.r.Tick(ctx)
	if g.client.n() != 0 {
		t.Error("まだ更新の時期ではない")
	}
	if st := g.r.Status(ctx); st.LastResult != "" || st.NextAttempt == nil || !st.NextAttempt.Equal(g.now.Add(licensing.CheckEvery)) {
		t.Errorf("%+v", st)
	}
}

func TestRenewer_自動更新が切れていれば通信しない(t *testing.T) {
	g := dueRig(t)
	g.set.v[licensing.AutoRenewSetting] = "false"
	g.r.Tick(ctx)
	g.now = t0.Add(time.Hour)
	g.r.Tick(ctx)
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrAutoRenewOff) {
		t.Errorf("Check now: %v", err)
	}
	if g.client.n() != 0 {
		t.Error("オフのときは通信しない")
	}
	if st := g.r.Status(ctx); st.Enabled || st.NextAttempt != nil {
		t.Errorf("%+v", st)
	}
}

func TestRenewer_サブスクリプションのない鍵は更新しない(t *testing.T) {
	l := subLicense(t0.AddDate(0, 0, 1))
	l.Subscription, l.RenewURL, l.Billing = "", "", ""
	g := newRig(t, l)
	g.r.Tick(ctx)
	g.now = t0.Add(licensing.FirstCheckAfter)
	g.r.Tick(ctx)
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrNotRenewable) || g.client.n() != 0 {
		t.Errorf("%v %d", err, g.client.n())
	}
	delete(g.set.v, licensing.KeySetting)
	g.now = g.now.Add(time.Hour)
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrNotRenewable) {
		t.Errorf("鍵なし: %v", err)
	}
}

func TestRenewer_リーダーだけが動く(t *testing.T) {
	g := dueRig(t)
	g.leader = false
	g.client.err = licensing.ErrNoNewerKey
	g.r.Tick(ctx)
	g.now = t0.Add(time.Hour)
	g.r.Tick(ctx)
	if g.client.n() != 0 {
		t.Fatal("フォロワーは通信しない")
	}
	if st := g.r.Status(ctx); st.LastResult != licensing.ResultUnknown || !st.Enabled {
		t.Errorf("%+v", st)
	}
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrNotLeader) {
		t.Errorf("%v", err)
	}
	// リーダーになってから約1分後に最初の確認をする。
	g.leader = true
	g.r.Tick(ctx)
	g.now = g.now.Add(licensing.FirstCheckAfter - time.Second)
	g.r.Tick(ctx)
	if g.client.n() != 0 {
		t.Error("1分たつまで待つ")
	}
	g.now = g.now.Add(time.Second)
	g.r.Tick(ctx)
	if g.client.n() != 1 || g.r.Status(ctx).LastResult != licensing.ResultNotYet {
		t.Errorf("%d %+v", g.client.n(), g.r.Status(ctx))
	}
	// リーダーでなくなると次にリーダーになったとき、また1分待つ。
	g.leader = false
	g.r.Tick(ctx)
	g.leader = true
	g.r.Tick(ctx)
	g.now = g.now.Add(time.Second)
	g.r.Tick(ctx)
	if g.client.n() != 1 {
		t.Error("再びリーダーになった直後は待つ")
	}
}

func TestRenewer_更新の結果ごとに記録し次の確認を決める(t *testing.T) {
	g := dueRig(t)
	g.client.err = licensing.ErrNoNewerKey
	st, _ := g.r.CheckNow(ctx)
	if st.LastResult != licensing.ResultNotYet || !st.NextAttempt.Equal(t0.Add(licensing.CheckEvery)) || len(g.told) != 0 {
		t.Errorf("204: %+v %v", st, g.told)
	}

	g.now = t0.Add(time.Minute)
	g.client.err = fmt.Errorf("%w: card declined", licensing.ErrSubscriptionInactive)
	st, _ = g.r.CheckNow(ctx)
	if st.LastResult != licensing.ResultInactive || !strings.Contains(st.Message, "card declined") {
		t.Errorf("402: %+v", st)
	}
	if len(g.told) != 1 || g.told[0].event != "license.renewal_inactive" || g.told[0].subject != "L1" {
		t.Fatalf("通知: %+v", g.told)
	}
	g.now = g.now.Add(time.Minute)
	g.r.CheckNow(ctx)
	if len(g.told) != 1 {
		t.Errorf("同じ期限については一度だけ通知する: %+v", g.told)
	}

	g.client.err = errors.New("dial tcp: connection refused")
	g.now = t0.Add(10 * time.Minute)
	st, _ = g.r.CheckNow(ctx)
	if st.LastResult != licensing.ResultError || !st.NextAttempt.Equal(g.now.Add(licensing.RetryFirst)) {
		t.Errorf("1回目の失敗は1時間後: %+v", st)
	}
	g.now = g.now.Add(licensing.ManualEvery)
	st, _ = g.r.CheckNow(ctx)
	if !st.NextAttempt.Equal(g.now.Add(licensing.RetryAgain)) {
		t.Errorf("続けて失敗したら6時間後: %+v", st)
	}
	g.client.err = licensing.ErrNoNewerKey
	g.now = g.now.Add(licensing.ManualEvery)
	st, _ = g.r.CheckNow(ctx)
	g.client.err = errors.New("again")
	g.now = g.now.Add(licensing.ManualEvery)
	if st, _ = g.r.CheckNow(ctx); !st.NextAttempt.Equal(g.now.Add(licensing.RetryFirst)) {
		t.Errorf("成功のあとは数え直す: %+v", st)
	}
}

func TestRenewer_ジッターは12時間に足される(t *testing.T) {
	g := dueRig(t)
	g.r.Jitter = func() time.Duration { return 40 * time.Minute }
	g.client.err = licensing.ErrNoNewerKey
	st, _ := g.r.CheckNow(ctx)
	if want := t0.Add(licensing.CheckEvery + 40*time.Minute); !st.NextAttempt.Equal(want) {
		t.Errorf("%v", st.NextAttempt)
	}
	g.r.Jitter = nil // 既定は±1時間
	g.now = t0.Add(time.Minute)
	st, _ = g.r.CheckNow(ctx)
	d := st.NextAttempt.Sub(g.now)
	if d < licensing.CheckEvery-licensing.CheckJitter || d > licensing.CheckEvery+licensing.CheckJitter {
		t.Errorf("%v", d)
	}
}

func TestRenewer_猶予期間に入って更新できなければ通知する(t *testing.T) {
	g := newRig(t, subLicense(t0.AddDate(0, 0, -2)))
	g.client.err = errors.New("timeout")
	g.r.CheckNow(ctx)
	g.now = g.now.Add(licensing.ManualEvery)
	g.r.CheckNow(ctx)
	if len(g.told) != 1 || g.told[0].event != "license.grace" || g.told[0].level != "error" || !strings.Contains(g.told[0].text, "grace") {
		t.Errorf("%+v", g.told)
	}
	// 期限前なら猶予の通知は出ない。
	h := dueRig(t)
	h.client.err = errors.New("timeout")
	h.r.CheckNow(ctx)
	if len(h.told) != 0 {
		t.Errorf("%+v", h.told)
	}
	// 通知先がなくても動く。
	h.r.Notify = nil
	h.now = h.now.Add(licensing.ManualEvery)
	h.client.err = licensing.ErrSubscriptionInactive
	if st, _ := h.r.CheckNow(ctx); st.LastResult != licensing.ResultInactive {
		t.Errorf("%+v", st)
	}
}

func TestRenewer_受け入れられない鍵は捨てて今の鍵を残す(t *testing.T) {
	good := subLicense(t0.AddDate(0, 1, 3))
	cases := []struct {
		name string
		make func(g *rig) string
		want string
	}{
		{"別のライセンス", func(g *rig) string { l := good; l.ID = "L9"; return g.issue(t, l) }, "licence"},
		{"期限が古い", func(g *rig) string { return g.issue(t, subLicense(t0.AddDate(0, 0, 1))) }, "not later"},
		{"有効なモジュールが減る", func(g *rig) string { l := good; l.Modules = []string{"auth"}; return g.issue(t, l) }, "audit"},
		{"別の根で署名", func(g *rig) string {
			other := &rig{root: license.NewLocalSigner()}
			return other.issue(t, good)
		}, "does not verify"},
		{"壊れた鍵", func(*rig) string { return "dbbl1.nope" }, "does not verify"},
		{"期限切れの鍵", func(g *rig) string {
			l := good
			l.Expires = t0.AddDate(0, 0, 3).Add(time.Hour)
			g.now = t0.AddDate(0, 5, 0)
			return g.issue(t, l)
		}, "not usable"},
	}
	for _, c := range cases {
		g := dueRig(t)
		old := g.savedKey()
		g.client.key = c.make(g)
		st, err := g.r.CheckNow(ctx)
		if err != nil || st.LastResult != licensing.ResultError || !strings.Contains(st.Message, c.want) {
			t.Errorf("%s: %+v %v", c.name, st, err)
		}
		if g.savedKey() != old {
			t.Errorf("%s: 鍵が置き換わった", c.name)
		}
	}
}

func TestRenewer_保存や一覧の失敗は結果に残す(t *testing.T) {
	g := dueRig(t)
	g.client.key = g.issue(t, subLicense(t0.AddDate(0, 1, 3)))
	g.set.putFail = errors.New("no quorum")
	if st, _ := g.r.CheckNow(ctx); st.LastResult != licensing.ResultError || !strings.Contains(st.Message, "no quorum") {
		t.Errorf("%+v", st)
	}
	g.set.putFail = nil
	g.mods.listFail = errors.New("store down")
	g.now = g.now.Add(licensing.ManualEvery)
	if st, _ := g.r.CheckNow(ctx); !strings.Contains(st.Message, "store down") {
		t.Errorf("%+v", st)
	}
	g.mods.listFail = nil
	g.r.Service.Modules = nil // モジュール管理がない構成でも更新できる
	g.now = g.now.Add(licensing.ManualEvery)
	if st, _ := g.r.CheckNow(ctx); st.LastResult != licensing.ResultRenewed {
		t.Errorf("%+v", st)
	}
}

func TestRenewer_httpの更新URLには通信せず根のない版や壊れた保存鍵は更新しない(t *testing.T) {
	l := subLicense(t0.AddDate(0, 0, 3))
	l.RenewURL = "http://licenses.example.com/renew"
	g := newRig(t, l)
	st, _ := g.r.CheckNow(ctx)
	if g.client.n() != 0 || st.LastResult != licensing.ResultError || !strings.Contains(st.Message, "https") {
		t.Errorf("%d %+v", g.client.n(), st)
	}

	g = dueRig(t)
	g.set.v[licensing.KeySetting] = "dbbl1.garbage"
	if _, err := g.r.CheckNow(ctx); err == nil || g.client.n() != 0 {
		t.Errorf("壊れた鍵: %v", err)
	}
	g = dueRig(t)
	g.r.Service.Root = nil
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrNoRoot) {
		t.Errorf("根なし: %v", err)
	}
	g.r.Tick(ctx) // ログに残るだけ
}

func TestRenewer_同時に二つの要求を出さず確認ボタンの連打を断る(t *testing.T) {
	g := dueRig(t)
	g.client.block = make(chan struct{})
	g.client.err = licensing.ErrNoNewerKey
	done := make(chan error)
	go func() { _, err := g.r.CheckNow(ctx); done <- err }()
	for g.client.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	g.now = g.now.Add(time.Hour)
	if _, err := g.r.CheckNow(ctx); !errors.Is(err, licensing.ErrInFlight) {
		t.Errorf("実行中: %v", err)
	}
	g.r.Tick(ctx)
	close(g.client.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if g.client.n() != 1 {
		t.Errorf("要求は1回だけ: %d", g.client.n())
	}
	var tooSoon *licensing.TooSoonError
	g.now = g.now.Add(time.Hour)
	g.r.CheckNow(ctx)
	if _, err := g.r.CheckNow(ctx); !errors.As(err, &tooSoon) || tooSoon.RetryAfter <= 0 || !strings.Contains(err.Error(), "try again") {
		t.Errorf("連打: %v", err)
	}
}

func TestRenewer_サービスのメッセージは短く一行にする(t *testing.T) {
	g := dueRig(t)
	g.client.err = fmt.Errorf("%w: %s\nline2\x00%s", licensing.ErrSubscriptionInactive, "x", strings.Repeat("あ", 400))
	st, _ := g.r.CheckNow(ctx)
	if strings.ContainsAny(st.Message, "\n\x00") || len([]rune(st.Message)) > 310 {
		t.Errorf("%q", st.Message)
	}
}

func TestRenewer_Runは終了までループする(t *testing.T) {
	g := dueRig(t)
	g.client.err = licensing.ErrNoNewerKey
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { g.r.Run(c); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run が終わらない")
	}
	if !g.r.Leading() {
		t.Error("Leading")
	}
}

func TestSave_使える鍵だけを保存しモジュールは有効にしない(t *testing.T) {
	s, root, mods, set := setup(t)
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(0, 1, 0)})
	if _, err := s.Save(ctx, key); err != nil || set.put["license.key"] != key || len(mods.installed) != 0 {
		t.Fatalf("%v %v %v", err, set.put, mods.installed)
	}
	old := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(0, -1, 0)})
	if _, err := s.Save(ctx, old); !errors.Is(err, licensing.ErrUnusable) {
		t.Errorf("%v", err)
	}
	s.Root = nil
	if _, err := s.Save(ctx, key); !errors.Is(err, licensing.ErrNoRoot) {
		t.Errorf("%v", err)
	}
}

func TestRenewer_サービスが待つよう求めたら通知せず失敗にも数えずその後まで待つ(t *testing.T) {
	g := newRig(t, subLicense(t0.AddDate(0, 0, -2))) // 猶予期間
	g.client.err = &licensing.RetryLaterError{After: 3 * time.Hour}
	st, err := g.r.CheckNow(ctx)
	if err != nil || st.LastResult != licensing.ResultNotYet || !st.NextAttempt.Equal(t0.Add(3*time.Hour)) || len(g.told) != 0 {
		t.Fatalf("%+v %v %v", st, err, g.told)
	}
	if !strings.Contains(st.Message, "try again") {
		t.Errorf("%q", st.Message)
	}
	g.now = t0.Add(time.Hour)
	g.client.err = &licensing.RetryLaterError{After: time.Second}
	if st, _ = g.r.CheckNow(ctx); !st.NextAttempt.Equal(g.now.Add(time.Minute)) {
		t.Errorf("最短でも1分: %v", st.NextAttempt)
	}
	g.now = g.now.Add(licensing.ManualEvery)
	g.client.err = errors.New("x")
	if st, _ = g.r.CheckNow(ctx); !st.NextAttempt.Equal(g.now.Add(licensing.RetryFirst)) {
		t.Errorf("待たされても失敗の数には入らない: %v", st.NextAttempt)
	}
}

func TestRenewer_期限が延びないプラン変更の鍵も受け入れノード上限を超えれば知らせる(t *testing.T) {
	g := dueRig(t) // 3 ノード、上限 5
	l := subLicense(t0.AddDate(0, 0, 3))
	l.IssuedAt, l.MaxNodes, l.Plan = t0.Add(time.Minute), 2, license.Team
	g.client.key = g.issue(t, l)
	st, _ := g.r.CheckNow(ctx)
	if st.LastResult != licensing.ResultRenewed || g.savedKey() != g.client.key || !strings.Contains(st.Message, "allows 2 nodes but the cluster has 3") {
		t.Errorf("%+v", st)
	}
	// 期限切れの鍵は上限超過でも受け入れない。
	h := dueRig(t)
	e := subLicense(t0.AddDate(0, 0, -30))
	e.IssuedAt, e.MaxNodes = t0.Add(time.Minute), 2
	h.client.key = h.issue(t, e)
	if st, _ := h.r.CheckNow(ctx); st.LastResult != licensing.ResultError {
		t.Errorf("%+v", st)
	}
}

func TestSaveRenewed_上限超過の鍵も保存するが期限切れは断る(t *testing.T) {
	s, root, _, set := setup(t) // 3 ノード
	over := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, MaxNodes: 2, IssuedAt: t0, Expires: t0.AddDate(0, 1, 0)})
	if _, err := s.Save(ctx, over); !errors.Is(err, licensing.ErrUnusable) {
		t.Errorf("Save: %v", err)
	}
	if _, err := s.SaveRenewed(ctx, over); err != nil || set.put["license.key"] != over {
		t.Errorf("SaveRenewed: %v", err)
	}
	old := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, MaxNodes: 2, IssuedAt: t0, Expires: t0.AddDate(0, -1, 0)})
	if _, err := s.SaveRenewed(ctx, old); !errors.Is(err, licensing.ErrUnusable) {
		t.Errorf("%v", err)
	}
}

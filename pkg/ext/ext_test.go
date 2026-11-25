package ext_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

func TestAllowAuthenticated_ログインしていれば何でも許す(t *testing.T) {
	a := ext.AllowAuthenticated{}
	if err := a.Authorize(ctx, ext.Principal{ID: "admin"}, "branch.delete", ext.Resource{Kind: "branch", Name: "x"}); err != nil {
		t.Error(err)
	}
	if err := a.Authorize(ctx, ext.Principal{}, "branch.read", ext.Resource{}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("anonymous: %v", err)
	}
}

type sink struct {
	got []ext.Event
	err error
}

func (s *sink) Record(_ context.Context, e ext.Event) error { s.got = append(s.got, e); return s.err }

func TestSinks_全部に配り失敗をまとめて返す(t *testing.T) {
	a, b := &sink{}, &sink{err: errors.New("disk full")}
	err := ext.Sinks{a, b}.Record(ctx, ext.Event{Action: "branch.create"})
	if len(a.got) != 1 || len(b.got) != 1 || err == nil {
		t.Errorf("a %v b %v err %v", a.got, b.got, err)
	}
	if err := (ext.Sinks{}).Record(ctx, ext.Event{}); err != nil {
		t.Error(err)
	}
}

type authFunc func(ext.Credentials) (ext.Principal, error)

func (f authFunc) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	return f(c)
}

func TestChain_扱えない資格情報は次へ回す(t *testing.T) {
	tokens := authFunc(func(c ext.Credentials) (ext.Principal, error) {
		if c.Token == "" {
			return ext.Principal{}, ext.ErrUnauthenticated
		}
		return ext.Principal{ID: "token:" + c.Token}, nil
	})
	passwords := authFunc(func(c ext.Credentials) (ext.Principal, error) {
		if c.Password != "pw" {
			return ext.Principal{}, errors.New("wrong password")
		}
		return ext.Principal{ID: c.Username}, nil
	})
	chain := ext.Chain{tokens, passwords}
	if p, err := chain.Authenticate(ctx, ext.Credentials{Token: "t1"}); err != nil || p.ID != "token:t1" {
		t.Errorf("token: %+v %v", p, err)
	}
	if p, err := chain.Authenticate(ctx, ext.Credentials{Username: "admin", Password: "pw"}); err != nil || p.ID != "admin" {
		t.Errorf("password: %+v %v", p, err)
	}
	if _, err := chain.Authenticate(ctx, ext.Credentials{Username: "admin", Password: "no"}); err == nil || errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("a decided failure must not fall through: %v", err)
	}
	if _, err := (ext.Chain{tokens}).Authenticate(ctx, ext.Credentials{}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("nobody: %v", err)
	}
}

type deny string

func (d deny) Authorize(_ context.Context, _ ext.Principal, action string, _ ext.Resource) error {
	if action == string(d) {
		return ext.ErrForbidden
	}
	return nil
}

func TestCombine_認証は順に試し認可は全員の許可が要り監査は全員に届く(t *testing.T) {
	if m := ext.Combine(nil); m.Authenticator != nil || m.Authorizer != nil || m.AuditSink != nil {
		t.Errorf("no modules: %+v", m)
	}
	s1, s2 := &sink{}, &sink{}
	m := ext.Combine([]ext.Module{
		{Authorizer: deny("branch.delete"), AuditSink: s1},
		{Authenticator: authFunc(func(c ext.Credentials) (ext.Principal, error) { return ext.Principal{ID: "x"}, nil }), Authorizer: deny("base.delete"), AuditSink: s2},
	})
	if p, _ := m.Authenticator.Authenticate(ctx, ext.Credentials{}); p.ID != "x" {
		t.Errorf("authenticator: %+v", p)
	}
	for action, want := range map[string]bool{"branch.delete": false, "base.delete": false, "branch.read": true} {
		if err := m.Authorizer.Authorize(ctx, ext.Principal{ID: "x"}, action, ext.Resource{}); (err == nil) != want {
			t.Errorf("%s: %v", action, err)
		}
	}
	_ = m.AuditSink.Record(ctx, ext.Event{})
	if len(s1.got) != 1 || len(s2.got) != 1 {
		t.Errorf("sinks %d %d", len(s1.got), len(s2.got))
	}
}

type router struct{ n int }

func (r *router) Route(context.Context, ext.RouteConfig) error { r.n++; return nil }

func TestCombine_振り分けは最初のモジュールのものを使う(t *testing.T) {
	a, b := &router{}, &router{}
	m := ext.Combine([]ext.Module{{Name: "audit"}, {Router: a, Ports: map[string]int{"mysql": 6033}}, {Router: b}})
	_ = m.Router.Route(ctx, ext.RouteConfig{})
	if a.n != 1 || b.n != 0 || m.Ports["mysql"] != 6033 {
		t.Errorf("a %d b %d ports %v", a.n, b.n, m.Ports)
	}
	if ext.Combine([]ext.Module{{Name: "x"}}).Router != nil {
		t.Error("no router: want nil")
	}
}

// pwAuth is an authenticator whose users (IDs with its prefix) change passwords.
type pwAuth struct {
	prefix  string
	changed *[]string
}

func (pwAuth) Authenticate(context.Context, ext.Credentials) (ext.Principal, error) {
	return ext.Principal{}, ext.ErrUnauthenticated
}

func (a pwAuth) ChangePassword(_ context.Context, p ext.Principal, _, next string) error {
	if !strings.HasPrefix(p.ID, a.prefix) {
		return ext.ErrUnauthenticated
	}
	*a.changed = append(*a.changed, p.ID+"="+next)
	return nil
}

type plainAuth struct{}

func (plainAuth) Authenticate(context.Context, ext.Credentials) (ext.Principal, error) {
	return ext.Principal{}, ext.ErrUnauthenticated
}

func TestCombine_パスワードを変えられる認証を引き継ぐ(t *testing.T) {
	var changed []string
	all := ext.Combine([]ext.Module{{Authenticator: plainAuth{}}, {Authenticator: pwAuth{"user:", &changed}}, {Authenticator: pwAuth{"sso:", &changed}}})
	pc, ok := all.Authenticator.(ext.PasswordChanger)
	if !ok {
		t.Fatal("combined authenticator should change passwords")
	}
	if err := pc.ChangePassword(ctx, ext.Principal{ID: "sso:bob"}, "a", "b"); err != nil || len(changed) != 1 || changed[0] != "sso:bob=b" {
		t.Errorf("%v %v", err, changed)
	}
	if err := pc.ChangePassword(ctx, ext.Principal{ID: "token:x"}, "a", "b"); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("nobody's user: %v", err)
	}
	if _, ok := ext.Combine([]ext.Module{{Authenticator: plainAuth{}}}).Authenticator.(ext.PasswordChanger); ok {
		t.Error("no module changes passwords")
	}
}

type pol struct {
	deny  error
	share bool
}

func (p pol) CheckBranch(context.Context, ext.Principal, ext.BranchRequest) error { return p.deny }
func (p pol) SharesBranches(context.Context, ext.Principal, string) bool          { return p.share }

type extendPol struct {
	pol
	extendDeny error
}

func (p extendPol) CheckExtend(context.Context, ext.Principal, ext.BranchRequest) error {
	return p.extendDeny
}

type roleResolver struct {
	prefix string
	role   string
	deny   error
}

func (r roleResolver) ResolvePrincipal(_ context.Context, p ext.Principal) (ext.Principal, error) {
	if r.deny != nil {
		return ext.Principal{}, r.deny
	}
	if r.prefix == "" || len(p.ID) >= len(r.prefix) && p.ID[:len(r.prefix)] == r.prefix {
		out := p
		out.Roles = []string{r.role}
		return out, nil
	}
	return p, nil
}

func TestCombine_ブランチの方針は全員が許せば作れどれかが許せば共有する(t *testing.T) {
	no := errors.New("no")
	all := ext.Combine([]ext.Module{{BranchPolicy: pol{}}, {BranchPolicy: pol{share: true}}})
	if err := all.BranchPolicy.CheckBranch(ctx, ext.Principal{}, ext.BranchRequest{}); err != nil {
		t.Errorf("allowed: %v", err)
	}
	if !all.BranchPolicy.SharesBranches(ctx, ext.Principal{}, "x") {
		t.Error("one shares")
	}
	ep, ok := all.BranchPolicy.(ext.BranchExtendPolicy)
	if !ok {
		t.Fatal("combined BranchPolicy implements BranchExtendPolicy")
	}
	if err := ep.CheckExtend(ctx, ext.Principal{}, ext.BranchRequest{}); err != nil {
		t.Errorf("extend allowed: %v", err)
	}
	deny := ext.Combine([]ext.Module{{BranchPolicy: pol{}}, {BranchPolicy: pol{deny: no}}})
	if err := deny.BranchPolicy.CheckBranch(ctx, ext.Principal{}, ext.BranchRequest{}); !errors.Is(err, no) {
		t.Errorf("denied: %v", err)
	}
	if deny.BranchPolicy.SharesBranches(ctx, ext.Principal{}, "x") {
		t.Error("none shares")
	}
	extDeny := ext.Combine([]ext.Module{{BranchPolicy: pol{}}, {BranchPolicy: extendPol{extendDeny: no}}})
	if err := extDeny.BranchPolicy.(ext.BranchExtendPolicy).CheckExtend(ctx, ext.Principal{}, ext.BranchRequest{}); !errors.Is(err, no) {
		t.Errorf("extend denied: %v", err)
	}
	if ext.Combine(nil).BranchPolicy != nil {
		t.Error("no policy")
	}
}

func TestCombine_主体解決は順に引き継ぎ拒否されれば止まる(t *testing.T) {
	if ext.Combine(nil).PrincipalResolver != nil {
		t.Error("no resolver")
	}
	all := ext.Combine([]ext.Module{
		{PrincipalResolver: roleResolver{prefix: "sso:", role: "viewer"}},
		{PrincipalResolver: roleResolver{prefix: "user:", role: "developer"}},
	})
	p, err := all.PrincipalResolver.ResolvePrincipal(ctx, ext.Principal{ID: "user:a", Roles: []string{"admin"}})
	if err != nil || len(p.Roles) != 1 || p.Roles[0] != "developer" {
		t.Errorf("resolved = %+v %v", p, err)
	}
	deny := ext.Combine([]ext.Module{
		{PrincipalResolver: roleResolver{prefix: "user:", role: "developer"}},
		{PrincipalResolver: roleResolver{deny: ext.ErrUnauthenticated}},
	})
	if _, err := deny.PrincipalResolver.ResolvePrincipal(ctx, ext.Principal{ID: "user:a"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("denied = %v", err)
	}
}

type follower struct {
	runs []ext.MaskRun
	err  error
	got  [2]string
}

func (f *follower) AfterRefresh(_ context.Context, base, snapshot string) ([]ext.MaskRun, error) {
	f.got = [2]string{base, snapshot}
	return f.runs, f.err
}

func TestCombine_更新後のマスク実行はモジュールをまたいで連結し一つの失敗で他を止めない(t *testing.T) {
	a := &follower{runs: []ext.MaskRun{{Target: "dev", Masker: "greenmask"}}}
	bad := &follower{err: errors.New("licence expired"), runs: []ext.MaskRun{{Target: "ignored"}}}
	c := &follower{runs: []ext.MaskRun{{Target: "qa", Masker: "anon"}}}
	out := ext.Combine([]ext.Module{{Name: "a", RefreshFollower: a}, {Name: "none"}, {Name: "bad", RefreshFollower: bad}, {Name: "c", RefreshFollower: c}})
	runs, err := out.RefreshFollower.AfterRefresh(ctx, "prod", "s1")
	want := []ext.MaskRun{{Target: "dev", Masker: "greenmask"}, {Target: "qa", Masker: "anon"}}
	if err != nil || len(runs) != 2 || runs[0] != want[0] || runs[1] != want[1] {
		t.Errorf("runs %v err %v", runs, err)
	}
	if c.got != [2]string{"prod", "s1"} {
		t.Errorf("got %v", c.got)
	}
}

func TestCombine_更新後のマスク実行を持つモジュールがなければ空のまま(t *testing.T) {
	if out := ext.Combine([]ext.Module{{Name: "a"}}); out.RefreshFollower != nil {
		t.Errorf("%v", out.RefreshFollower)
	}
}

type optioner struct {
	lines  []string
	err    error
	record bool
}

func (o optioner) ServerOptions(context.Context, ext.ServerBranch) (ext.ServerOptions, error) {
	return ext.ServerOptions{Config: o.lines, Record: o.record}, o.err
}

func TestCombine_サーバーの追加設定は全モジュール分をつなぎ一つの失敗で全体が失敗する(t *testing.T) {
	out := ext.Combine([]ext.Module{{ServerOptioner: optioner{lines: []string{"a=1"}}}, {Name: "none"}, {ServerOptioner: optioner{lines: []string{"b=2"}}}})
	got, err := out.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{Name: "x"})
	if err != nil || len(got.Config) != 2 || got.Config[0] != "a=1" || got.Config[1] != "b=2" {
		t.Errorf("got %v %v", got, err)
	}
	no := errors.New("no licence")
	bad := ext.Combine([]ext.Module{{ServerOptioner: optioner{lines: []string{"a=1"}}}, {ServerOptioner: optioner{err: no}}})
	if _, err := bad.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{}); !errors.Is(err, no) {
		t.Errorf("err = %v", err)
	}
	if ext.Combine([]ext.Module{{Name: "a"}}).ServerOptioner != nil {
		t.Error("no optioner")
	}
}

func TestCombine_新しいブランチを記録するかはどれか一つのモジュールが望めば記録になる(t *testing.T) {
	out := ext.Combine([]ext.Module{{ServerOptioner: optioner{}}, {ServerOptioner: optioner{record: true}}})
	got, err := out.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{Name: "x", Creating: true})
	if err != nil || !got.Record {
		t.Errorf("got %+v %v", got, err)
	}
	none := ext.Combine([]ext.Module{{ServerOptioner: optioner{}}, {ServerOptioner: optioner{}}})
	if got, _ := none.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{}); got.Record {
		t.Error("nobody asked to record")
	}
}

type finisher struct {
	err  error
	note *string
}

func (f finisher) AfterRestore(_ context.Context, _ ext.ServerBranch, note string) error {
	if f.note != nil {
		*f.note += note
	}
	return f.err
}

func TestCombine_復元後の処理は全モジュールが走り失敗はまとめて返す(t *testing.T) {
	var note string
	boom := errors.New("boom")
	out := ext.Combine([]ext.Module{{RestoreFinisher: finisher{err: boom}}, {RestoreFinisher: finisher{note: &note}}})
	if err := out.RestoreFinisher.AfterRestore(ctx, ext.ServerBranch{}, "n"); !errors.Is(err, boom) || note != "n" {
		t.Errorf("err %v note %q: the second module must still run", err, note)
	}
	if ext.Combine([]ext.Module{{Name: "a"}}).RestoreFinisher != nil {
		t.Error("no finisher")
	}
}

// legacyDBAccess is DBAccess as modules built before Endpoints and Env knew it.
type legacyDBAccess struct {
	Engine, Host, Socket string
	Port                 int
	User, BinDir, LibDir string
}

func TestDBAccess_Endpointsとenvを足しても古いモジュールと互いに読める(t *testing.T) {
	now := ext.DBAccess{Engine: "clickhouse", Host: "127.0.0.1", Socket: "/run/ch.sock", Port: 8123, User: "default", BinDir: "/rt/bin", LibDir: "/rt/lib",
		Endpoints: map[string]ext.Endpoint{"http": {Host: "127.0.0.1", Port: 8123}, "native": {Host: "127.0.0.1", Port: 9000, Socket: "/run/ch-native.sock"}},
		Env:       map[string]string{"CLICKHOUSE_HOST": "127.0.0.1"}}

	// the agent (new) calls a module built with the old type: gob drops what the old type lacks
	var wire bytes.Buffer
	if err := gob.NewEncoder(&wire).Encode(now); err != nil {
		t.Fatal(err)
	}
	var old legacyDBAccess
	if err := gob.NewDecoder(&wire).Decode(&old); err != nil {
		t.Fatalf("an old module cannot read the new value: %v", err)
	}
	if old.Engine != "clickhouse" || old.Port != 8123 || old.LibDir != "/rt/lib" {
		t.Errorf("old module saw %+v", old)
	}

	// and the other way round: a value from an old agent has no Endpoints
	wire.Reset()
	if err := gob.NewEncoder(&wire).Encode(legacyDBAccess{Engine: "mysql", Host: "h", Port: 3306}); err != nil {
		t.Fatal(err)
	}
	var fresh ext.DBAccess
	if err := gob.NewDecoder(&wire).Decode(&fresh); err != nil || fresh.Engine != "mysql" || fresh.Endpoints != nil || fresh.Env != nil {
		t.Errorf("new module read an old value: %+v, %v", fresh, err)
	}
}

func TestDBAccess_JSONでは空のEndpointsとenvを出さない(t *testing.T) {
	b, _ := json.Marshal(ext.DBAccess{Engine: "mysql", Host: "h", Port: 3306})
	if strings.Contains(string(b), "endpoints") || strings.Contains(string(b), "env") {
		t.Errorf("json = %s", b)
	}
	b, _ = json.Marshal(ext.DBAccess{Engine: "x", Endpoints: map[string]ext.Endpoint{"http": {Host: "h", Port: 1}}, Env: map[string]string{"A": "b"}})
	if !strings.Contains(string(b), `"endpoints":{"http":{"host":"h","port":1}}`) || !strings.Contains(string(b), `"env":{"A":"b"}`) {
		t.Errorf("json = %s", b)
	}
}

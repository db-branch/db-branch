package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"github.com/db-branch/db-branch/pkg/ext"
)

func remoteFor(t *testing.T, m ext.Module) *Remote {
	t.Helper()
	c, _ := goplugin.TestPluginRPCConn(t, map[string]goplugin.Plugin{pluginName: &modulePlugin{impl: m}}, nil)
	t.Cleanup(func() { c.Close() })
	raw, err := c.Dispense(pluginName)
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*Remote)
}

func Test提供しないポートは既定どおりに振る舞う(t *testing.T) {
	r := remoteFor(t, ext.Module{Name: "nothing"})
	info, err := r.Info()
	if err != nil || info.Name != "nothing" || info.Authenticator || info.Authorizer || info.Sink {
		t.Errorf("info %+v %v", info, err)
	}
	ctx := context.Background()
	if _, err := r.Authenticate(ctx, ext.Credentials{Token: "x"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("authenticate: %v", err)
	}
	if err := r.Authorize(ctx, ext.Principal{ID: "a"}, "branch.read", ext.Resource{}); err != nil {
		t.Errorf("authorize: %v", err)
	}
	if err := r.Record(ctx, ext.Event{}); err != nil {
		t.Errorf("record: %v", err)
	}
	if err := r.Route(ctx, ext.RouteConfig{}); err != nil {
		t.Errorf("route: %v", err)
	}
}

type failingRouter struct{ got ext.RouteConfig }

func (f *failingRouter) Route(_ context.Context, c ext.RouteConfig) error {
	f.got = c
	return errors.New("proxysql down")
}

func Test振り分けの設定を渡し失敗を返す(t *testing.T) {
	fr := &failingRouter{}
	r := remoteFor(t, ext.Module{Name: "proxy", Router: fr, Ports: map[string]int{"mysql": 6033}})
	info, _ := r.Info()
	if !info.Router || info.Ports["mysql"] != 6033 {
		t.Errorf("info %+v", info)
	}
	cfg := ext.RouteConfig{MySQL: ext.RouteTables{Servers: []ext.RouteServer{{Hostgroup: 1001, Host: "h", Port: 1}}}}
	if err := r.Route(context.Background(), cfg); err == nil || err.Error() != "proxysql down" {
		t.Errorf("route: %v", err)
	}
	if len(fr.got.MySQL.Servers) != 1 || fr.got.MySQL.Servers[0].Hostgroup != 1001 {
		t.Errorf("module got %+v", fr.got)
	}
}

func Test接続が切れたら呼び出しはエラーになる(t *testing.T) {
	r := remoteFor(t, ext.Module{Name: "gone"})
	_ = r.c.Close()
	ctx := context.Background()
	if _, err := r.Authenticate(ctx, ext.Credentials{}); err == nil {
		t.Error("authenticate: want error")
	}
	if err := r.Authorize(ctx, ext.Principal{}, "", ext.Resource{}); err == nil {
		t.Error("authorize: want error")
	}
	if err := r.Record(ctx, ext.Event{}); err == nil {
		t.Error("record: want error")
	}
	if err := r.Route(ctx, ext.RouteConfig{}); err == nil {
		t.Error("route: want error")
	}
}

func TestRPCError_番兵を保って運ぶ(t *testing.T) {
	for _, err := range []error{nil, ext.ErrUnauthenticated, ext.ErrForbidden, errors.New("other")} {
		back := toRPC(err).err()
		if (err == nil) != (back == nil) || (err != nil && !errors.Is(back, err) && back.Error() != err.Error()) {
			t.Errorf("%v -> %v", err, back)
		}
	}
}

type cfg struct{ got map[string]string }

func (c *cfg) Configure(_ context.Context, v map[string]string) error {
	c.got = v
	if v["bad"] != "" {
		return errors.New("bad value")
	}
	return nil
}

func Test設定の定義と値を運ぶ(t *testing.T) {
	c := &cfg{}
	r := remoteFor(t, ext.Module{Name: "m", Settings: []ext.Setting{{Key: "k", Type: "bool", Default: "true", Translations: map[string]ext.SettingText{"ja": {Label: "鍵"}}}}, Configurable: c})
	info, _ := r.Info()
	if !info.Configurable || len(info.Settings) != 1 || info.Settings[0].Default != "true" || info.Settings[0].Translations["ja"].Label != "鍵" {
		t.Errorf("info %+v", info)
	}
	if err := r.Configure(context.Background(), map[string]string{"k": "false"}); err != nil || c.got["k"] != "false" {
		t.Errorf("configure: %v %v", err, c.got)
	}
	if err := r.Configure(context.Background(), map[string]string{"bad": "1"}); err == nil {
		t.Error("want error")
	}
	_ = r.c.Close()
	if err := r.Configure(context.Background(), nil); err == nil {
		t.Error("closed: want error")
	}
	n := remoteFor(t, ext.Module{Name: "plain"})
	if err := n.Configure(context.Background(), map[string]string{"x": "1"}); err != nil {
		t.Errorf("not configurable: %v", err)
	}
}

type echo struct{}

func (echo) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	if r.Path == "/fail" {
		return ext.Response{}, errors.New("broken")
	}
	return ext.Response{Status: 201, ContentType: "text/plain", Body: []byte(r.Method + " " + r.Path + "?" + r.RawQuery + " " + string(r.Body) + " " + r.Principal.ID)}, nil
}

func TestServe_モジュールのAPIに要求を渡す(t *testing.T) {
	r := remoteFor(t, ext.Module{Name: "m", Handler: echo{}, Pages: []ext.Page{{Path: "/ui/", Title: "Audit"}}})
	info, _ := r.Info()
	if !info.Handler || len(info.Pages) != 1 || info.Pages[0].Title != "Audit" {
		t.Errorf("info %+v", info)
	}
	resp, err := r.Serve(context.Background(), ext.Request{Method: "POST", Path: "/x", RawQuery: "a=1", Body: []byte("b"), Principal: ext.Principal{ID: "alice"}})
	if err != nil || resp.Status != 201 || string(resp.Body) != "POST /x?a=1 b alice" {
		t.Errorf("serve = %+v %v", resp, err)
	}
	if _, err := r.Serve(context.Background(), ext.Request{Path: "/fail"}); err == nil {
		t.Error("handler error: want error")
	}
	plain := remoteFor(t, ext.Module{Name: "plain"})
	if resp, err := plain.Serve(context.Background(), ext.Request{Path: "/"}); err != nil || resp.Status != 404 {
		t.Errorf("no handler: %+v %v", resp, err)
	}
	_ = r.c.Close()
	if _, err := r.Serve(context.Background(), ext.Request{}); err == nil {
		t.Error("closed: want error")
	}
}

type memStore map[string][]byte

func (m memStore) List(context.Context) (map[string][]byte, error) { return m, nil }
func (m memStore) Put(_ context.Context, k string, v []byte) error {
	if k == "bad" {
		return errors.New("too large")
	}
	m[k] = v
	return nil
}
func (m memStore) Delete(_ context.Context, k string) error {
	delete(m, k)
	return nil
}

type starter struct{ host chan ext.Host }

func (s starter) Start(_ context.Context, h ext.Host) error {
	if h.Node == "refuse" {
		return errors.New("no licence")
	}
	s.host <- h
	return nil
}

func TestStart_モジュールはエージェントの保存先を使える(t *testing.T) {
	ctx := context.Background()
	st := starter{host: make(chan ext.Host, 1)}
	r := remoteFor(t, ext.Module{Name: "m", Starter: st})
	if info, _ := r.Info(); !info.Starter {
		t.Errorf("info %+v", info)
	}
	data := memStore{"seed": []byte("1")}
	if err := r.Start(context.Background(), ext.Host{Node: "n1", DataDir: "/var/lib/x", Store: data}); err != nil {
		t.Fatal(err)
	}
	h := <-st.host
	if h.Node != "n1" || h.DataDir != "/var/lib/x" {
		t.Errorf("host %+v", h)
	}
	if err := h.Store.Put(ctx, "user/a", []byte("v")); err != nil || string(data["user/a"]) != "v" {
		t.Errorf("put: %v %v", err, data)
	}
	if err := h.Store.Put(ctx, "bad", nil); err == nil {
		t.Error("put error: want error")
	}
	if got, err := h.Store.List(ctx); err != nil || len(got) != 2 {
		t.Errorf("list = %v %v", got, err)
	}
	if err := h.Store.Delete(ctx, "user/a"); err != nil || data["user/a"] != nil {
		t.Errorf("delete: %v", err)
	}
	if err := r.Start(context.Background(), ext.Host{Node: "refuse", Store: data}); err == nil {
		t.Error("start error: want error")
	}
	plain := remoteFor(t, ext.Module{Name: "plain"})
	if err := plain.Start(context.Background(), ext.Host{Store: data}); err != nil {
		t.Errorf("no starter: %v", err)
	}
	h.Store.(*remoteStore).c.Close()
	if _, err := h.Store.List(ctx); err == nil {
		t.Error("closed list")
	}
	if err := h.Store.Put(ctx, "k", nil); err == nil {
		t.Error("closed put")
	}
	if err := h.Store.Delete(ctx, "k"); err == nil {
		t.Error("closed delete")
	}
	_ = r.c.Close()
	if err := r.Start(context.Background(), ext.Host{Store: data}); err == nil {
		t.Error("closed: want error")
	}
}

func Testパスワードを変えられないモジュールは変更を断る(t *testing.T) {
	r := remoteFor(t, ext.Module{Name: "nothing"})
	if info, _ := r.Info(); info.PasswordChanger {
		t.Error("no authenticator, no password changes")
	}
	err := passwordRemote{r}.ChangePassword(context.Background(), ext.Principal{ID: "x"}, "a", "b")
	if !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("change: %v", err)
	}
}

func Test種類の前置きを二重にしない(t *testing.T) {
	e := toRPC(fmt.Errorf("%w: team x is full", ext.ErrForbidden)).err()
	if e.Error() != "forbidden: team x is full" || !errors.Is(e, ext.ErrForbidden) {
		t.Errorf("%v", e)
	}
}

// memStorage records the calls of a module's storage.
type memStorage struct {
	calls []string
	fail  error
}

func (m *memStorage) rec(format string, a ...any) error {
	m.calls = append(m.calls, fmt.Sprintf(format, a...))
	return m.fail
}
func (m *memStorage) Create(_ context.Context, ds string, p map[string]string) error {
	return m.rec("create %s %s", ds, p["recordsize"])
}
func (m *memStorage) Snapshot(_ context.Context, ds, s string) error {
	return m.rec("snapshot %s@%s", ds, s)
}
func (m *memStorage) Clone(_ context.Context, ds, s, to string) error {
	return m.rec("clone %s@%s %s", ds, s, to)
}
func (m *memStorage) Rollback(_ context.Context, ds, s string) error {
	return m.rec("rollback %s@%s", ds, s)
}
func (m *memStorage) Destroy(_ context.Context, ds string) error { return m.rec("destroy %s", ds) }
func (m *memStorage) DestroySnapshot(_ context.Context, ds, s string) error {
	return m.rec("destroy %s@%s", ds, s)
}
func (m *memStorage) Exists(_ context.Context, ds string) (bool, error) {
	return ds == "fsx/db-branch", m.rec("exists %s", ds)
}
func (m *memStorage) Info(_ context.Context, ds string) (ext.DatasetInfo, error) {
	return ext.DatasetInfo{Mountpoint: "/mnt/" + ds, Used: 1, Referenced: 2}, m.rec("info %s", ds)
}
func (m *memStorage) Mounted(_ context.Context, parent string) ([]string, error) {
	return []string{"x"}, m.rec("mounted %s", parent)
}
func (m *memStorage) Release(_ context.Context, ds string) error { return m.rec("release %s", ds) }

func Testストレージの操作を運び失敗を返す(t *testing.T) {
	ms := &memStorage{}
	r := remoteFor(t, ext.Module{Name: "fsx", Storage: ms, SharedStorage: true})
	info, _ := r.Info()
	if !info.Storage || !info.SharedStorage {
		t.Fatalf("info %+v", info)
	}
	ctx := context.Background()
	st := storageRemote{r}
	_ = st.Create(ctx, "fsx/db-branch", map[string]string{"recordsize": "16K"})
	_ = st.Snapshot(ctx, "fsx/a", "s1")
	_ = st.Clone(ctx, "fsx/a", "s1", "fsx/b")
	_ = st.Rollback(ctx, "fsx/a", "s1")
	_ = st.DestroySnapshot(ctx, "fsx/a", "s1")
	_ = st.Destroy(ctx, "fsx/b")
	_ = st.Release(ctx, "fsx/b")
	ok, err := st.Exists(ctx, "fsx/db-branch")
	in, _ := st.Info(ctx, "fsx/a")
	names, _ := st.Mounted(ctx, "fsx/db-branch/branches")
	if !ok || err != nil || in.Mountpoint != "/mnt/fsx/a" || in.Referenced != 2 || len(names) != 1 {
		t.Errorf("exists %v %v info %+v mounted %v", ok, err, in, names)
	}
	want := "create fsx/db-branch 16K|snapshot fsx/a@s1|clone fsx/a@s1 fsx/b|rollback fsx/a@s1|destroy fsx/a@s1|destroy fsx/b|release fsx/b|exists fsx/db-branch|info fsx/a|mounted fsx/db-branch/branches"
	if got := fmt.Sprint(ms.calls); got != "["+strings.ReplaceAll(want, "|", " ")+"]" {
		t.Errorf("calls %s", got)
	}
	ms.fail = errors.New("volume busy")
	if err := st.Destroy(ctx, "fsx/a"); err == nil || !strings.Contains(err.Error(), "volume busy") {
		t.Errorf("failure: %v", err)
	}
	if _, err := st.Exists(ctx, "x"); err == nil {
		t.Error("exists failure")
	}
	none := storageRemote{remoteFor(t, ext.Module{Name: "plain"})}
	if err := none.Destroy(ctx, "x"); err == nil {
		t.Error("a module without storage")
	}
	_ = r.c.Close()
	if _, err := st.Info(ctx, "x"); err == nil {
		t.Error("closed")
	}
}

type recMasker struct{ got ext.MaskJob }

func (m *recMasker) Mask(_ context.Context, j ext.MaskJob) (string, error) {
	m.got = j
	if j.Rules == "bad" {
		return "", errors.New("greenmask: exit 1")
	}
	return "masked", nil
}

type recPolicy struct{ calls []string }

func (p *recPolicy) PrepareMask(_ context.Context, pl ext.MaskPlan) (string, error) {
	p.calls = append(p.calls, "prepare "+pl.Target+" "+pl.SourceDB.Socket)
	if pl.Rules == "refuse" {
		return "", fmt.Errorf("%w: unclassified columns", ext.ErrForbidden)
	}
	return "approved", nil
}
func (p *recPolicy) VerifyMask(_ context.Context, pl ext.MaskPlan) error {
	p.calls = append(p.calls, "verify "+pl.Target)
	return nil
}
func (p *recPolicy) MaskRecorded(_ context.Context, pl ext.MaskPlan, snap string) error {
	p.calls = append(p.calls, "recorded "+pl.Target+"@"+snap)
	return nil
}

func Testマスクとその方針を運ぶ(t *testing.T) {
	m, p := &recMasker{}, &recPolicy{}
	r := remoteFor(t, ext.Module{Name: "mask", Masker: m, MaskPolicy: p})
	info, _ := r.Info()
	if !info.Masker || !info.MaskPolicy {
		t.Fatalf("info %+v", info)
	}
	ctx := context.Background()
	job := ext.MaskJob{Source: ext.DBAccess{Engine: "postgres", Socket: "/run/a", Port: 1}, Target: ext.DBAccess{Socket: "/run/b"}, Rules: "r"}
	if log, err := r.Mask(ctx, job); err != nil || log != "masked" || m.got.Source.Socket != "/run/a" {
		t.Errorf("mask %q %v %+v", log, err, m.got)
	}
	if _, err := r.Mask(ctx, ext.MaskJob{Rules: "bad"}); err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("mask failure: %v", err)
	}
	plan := ext.MaskPlan{Target: "dev", SourceDB: ext.DBAccess{Socket: "/run/a"}}
	if rules, err := r.PrepareMask(ctx, plan); err != nil || rules != "approved" {
		t.Errorf("prepare %q %v", rules, err)
	}
	plan.Rules = "refuse"
	if _, err := r.PrepareMask(ctx, plan); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("refused: %v", err)
	}
	_ = r.VerifyMask(ctx, plan)
	_ = r.MaskRecorded(ctx, plan, "s1")
	if got := strings.Join(p.calls, "|"); got != "prepare dev /run/a|prepare dev /run/a|verify dev|recorded dev@s1" {
		t.Errorf("calls %s", got)
	}
	none := remoteFor(t, ext.Module{Name: "plain"})
	if _, err := none.Mask(ctx, job); err == nil {
		t.Error("a module without a masker")
	}
	if rules, err := none.PrepareMask(ctx, ext.MaskPlan{Rules: "x"}); err != nil || rules != "x" {
		t.Errorf("no policy keeps the rules: %q %v", rules, err)
	}
	if none.VerifyMask(ctx, plan) != nil || none.MaskRecorded(ctx, plan, "s") != nil {
		t.Error("no policy allows")
	}
	_ = r.c.Close()
	for _, err := range []error{r.VerifyMask(ctx, plan), r.MaskRecorded(ctx, plan, "s")} {
		if err == nil {
			t.Error("closed")
		}
	}
	if _, err := r.PrepareMask(ctx, plan); err == nil {
		t.Error("closed prepare")
	}
	if _, err := r.Mask(ctx, job); err == nil {
		t.Error("closed mask")
	}
}

func TestCombine_最初のマスク方針を使う(t *testing.T) {
	p := &recPolicy{}
	c := ext.Combine([]ext.Module{{Name: "a"}, {Name: "b", MaskPolicy: p}})
	if c.MaskPolicy != p {
		t.Errorf("%v", c.MaskPolicy)
	}
}

type recFollower struct {
	got [2]string
	err error
}

func (f *recFollower) AfterRefresh(_ context.Context, base, snap string) ([]ext.MaskRun, error) {
	f.got = [2]string{base, snap}
	return []ext.MaskRun{{Target: "dev", Masker: "greenmask"}}, f.err
}

func Test更新後のマスク実行を運ぶ(t *testing.T) {
	ctx := context.Background()
	f := &recFollower{}
	r := remoteFor(t, ext.Module{Name: "follow", RefreshFollower: f})
	if info, _ := r.Info(); !info.RefreshFollower {
		t.Fatalf("info %+v", info)
	}
	runs, err := r.AfterRefresh(ctx, "prod", "s1")
	if err != nil || len(runs) != 1 || runs[0] != (ext.MaskRun{Target: "dev", Masker: "greenmask"}) || f.got != [2]string{"prod", "s1"} {
		t.Errorf("runs %v err %v got %v", runs, err, f.got)
	}
	f.err = ext.ErrForbidden
	if _, err := r.AfterRefresh(ctx, "prod", "s1"); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("error: %v", err)
	}
	none := remoteFor(t, ext.Module{Name: "plain"})
	if info, _ := none.Info(); info.RefreshFollower {
		t.Error("a module without the port must not advertise it")
	}
	if runs, err := none.AfterRefresh(ctx, "prod", "s1"); err != nil || len(runs) != 0 {
		t.Errorf("no follower: %v %v", runs, err)
	}
	_ = r.c.Close()
	if _, err := r.AfterRefresh(ctx, "prod", "s1"); err == nil {
		t.Error("closed")
	}
}

type fakeFileInfo struct {
	mode fs.FileMode
}

func (fakeFileInfo) Name() string        { return "x" }
func (fakeFileInfo) Size() int64         { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode { return f.mode }
func (fakeFileInfo) ModTime() time.Time  { return time.Time{} }
func (f fakeFileInfo) IsDir() bool       { return f.mode.IsDir() }
func (fakeFileInfo) Sys() any            { return nil }

func TestFileOwner_実ファイルはUIDを返しStat情報がない場合は所有不明として拒否する(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := fileOwner(fi)
	if !ok || uid != uint32(os.Geteuid()) {
		t.Errorf("fileOwner(real dir) = (%d, %v), want (%d, true)", uid, ok, os.Geteuid())
	}
	if _, ok := fileOwner(fakeFileInfo{mode: 0o755}); ok {
		t.Error("fileOwner with nil Sys(): want ok=false")
	}
}

func TestClassifyOpenErr_各システムエラーを適切に分類する(t *testing.T) {
	for name, err := range map[string]error{
		"ELOOP":      syscall.ELOOP,
		"EMLINK":     syscall.EMLINK,
		"ENOTDIR":    syscall.ENOTDIR,
		"ENXIO":      syscall.ENXIO,
		"EOPNOTSUPP": syscall.EOPNOTSUPP,
	} {
		got := classifyOpenErr("/p", err)
		if !errors.Is(got, ErrUntrusted) || !errors.Is(got, err) {
			t.Errorf("%s: got %v, want ErrUntrusted wrapping %v", name, got, err)
		}
	}
	other := &os.PathError{Op: "open", Path: "/p", Err: syscall.EACCES}
	if got := classifyOpenErr("/p", other); got != other {
		t.Errorf("EACCES: got %v, want raw %v", got, other)
	}
}

func TestCheckTrustInfo_ファイル種別と特殊ビットをFS非依存で検証する(t *testing.T) {
	l := loader{ownerOf: func(fs.FileInfo) (uint32, bool) { return 0, true }}
	if err := l.checkTrustInfo("/p", fakeFileInfo{mode: 0o755}, true); !errors.Is(err, ErrUntrusted) {
		t.Errorf("non-dir with wantDir=true: got %v, want ErrUntrusted", err)
	}
	for name, mode := range map[string]fs.FileMode{
		"dir setuid": fs.ModeDir | fs.ModeSetuid | 0o755,
		"dir sticky": fs.ModeDir | fs.ModeSticky | 0o755,
	} {
		if err := l.checkTrustInfo("/d", fakeFileInfo{mode: mode}, true); !errors.Is(err, ErrUntrusted) {
			t.Errorf("%s: got %v, want ErrUntrusted", name, err)
		}
	}
	if err := l.checkTrustInfo("/d", fakeFileInfo{mode: fs.ModeDir | fs.ModeSetgid | 0o755}, true); err != nil {
		t.Errorf("dir setgid: got %v, want nil", err)
	}
	for name, mode := range map[string]fs.FileMode{
		"file setuid": fs.ModeSetuid | 0o755,
		"file setgid": fs.ModeSetgid | 0o755,
		"file sticky": fs.ModeSticky | 0o755,
	} {
		if err := l.checkTrustInfo("/f", fakeFileInfo{mode: mode}, false); !errors.Is(err, ErrUntrusted) {
			t.Errorf("%s: got %v, want ErrUntrusted", name, err)
		}
	}
}

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	if c.Token == "ok" {
		return ext.Principal{ID: "u1", Name: "alice"}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}
func (fakeAuth) ChangePassword(_ context.Context, _ ext.Principal, cur, next string) error {
	if cur != "old" {
		return ext.ErrUnauthenticated
	}
	if next == "" {
		return errors.New("empty")
	}
	return nil
}

type fakeAuthz struct{}

func (fakeAuthz) Authorize(_ context.Context, _ ext.Principal, action string, _ ext.Resource) error {
	if action == "deny" {
		return ext.ErrForbidden
	}
	return nil
}

type fakeSink struct{ got []string }

func (s *fakeSink) Record(_ context.Context, e ext.Event) error {
	s.got = append(s.got, e.Action)
	return nil
}

type fakePolicy struct{}

func (fakePolicy) CheckBranch(_ context.Context, _ ext.Principal, req ext.BranchRequest) error {
	if req.Base != "prod" {
		return ext.ErrForbidden
	}
	return nil
}
func (fakePolicy) SharesBranches(_ context.Context, _ ext.Principal, owner string) bool {
	return owner == "team:a"
}

type fakeExtendPolicy struct{ fakePolicy }

func (fakeExtendPolicy) CheckExtend(_ context.Context, _ ext.Principal, req ext.BranchRequest) error {
	if req.TTL > time.Hour {
		return ext.ErrForbidden
	}
	return nil
}

type fakeResolver struct{}

func (fakeResolver) ResolvePrincipal(_ context.Context, p ext.Principal) (ext.Principal, error) {
	if p.ID == "user:disabled" {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	if p.ID == "user:alice" {
		p.Roles = []string{"viewer"}
	}
	return p, nil
}

func Test認証と認可と監査とブランチ方針をRPC越しに運ぶ(t *testing.T) {
	sink := &fakeSink{}
	r := remoteFor(t, ext.Module{
		Name:              "sec",
		Authenticator:     fakeAuth{},
		Authorizer:        fakeAuthz{},
		AuditSink:         sink,
		BranchPolicy:      fakeExtendPolicy{},
		PrincipalResolver: fakeResolver{},
	})
	info, err := r.Info()
	if err != nil || !info.PrincipalResolver || !info.BranchExtendPolicy {
		t.Fatalf("info = %+v %v", info, err)
	}
	ctx := context.Background()
	p, err := r.Authenticate(ctx, ext.Credentials{Token: "ok"})
	if err != nil || p.ID != "u1" {
		t.Fatalf("Authenticate = %+v %v", p, err)
	}
	if resolved, err := r.ResolvePrincipal(ctx, ext.Principal{ID: "user:alice", Roles: []string{"admin"}}); err != nil || len(resolved.Roles) != 1 || resolved.Roles[0] != "viewer" {
		t.Errorf("ResolvePrincipal = %+v %v", resolved, err)
	}
	if _, err := r.ResolvePrincipal(ctx, ext.Principal{ID: "user:disabled"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("ResolvePrincipal disabled: %v", err)
	}
	if _, err := r.Authenticate(ctx, ext.Credentials{Token: "no"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("bad token: %v", err)
	}
	pc := passwordRemote{r}
	if err := pc.ChangePassword(ctx, p, "old", "new"); err != nil {
		t.Errorf("ChangePassword ok: %v", err)
	}
	if err := pc.ChangePassword(ctx, p, "bad", "new"); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("ChangePassword bad: %v", err)
	}
	if err := r.Authorize(ctx, p, "allow", ext.Resource{}); err != nil {
		t.Errorf("Authorize allow: %v", err)
	}
	if err := r.Authorize(ctx, p, "deny", ext.Resource{}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("Authorize deny: %v", err)
	}
	if err := r.Record(ctx, ext.Event{Action: "branch.create"}); err != nil || len(sink.got) != 1 {
		t.Errorf("Record: %v %v", err, sink.got)
	}
	if err := r.CheckBranch(ctx, p, ext.BranchRequest{Base: "prod"}); err != nil {
		t.Errorf("CheckBranch ok: %v", err)
	}
	if err := r.CheckBranch(ctx, p, ext.BranchRequest{Base: "other"}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("CheckBranch deny: %v", err)
	}
	if !r.SharesBranches(ctx, p, "team:a") || r.SharesBranches(ctx, p, "solo") {
		t.Error("SharesBranches mismatch")
	}
	ep := extendPolicyRemote{r}
	if err := ep.CheckExtend(ctx, p, ext.BranchRequest{TTL: 30 * time.Minute}); err != nil {
		t.Errorf("CheckExtend ok: %v", err)
	}
	if err := ep.CheckExtend(ctx, p, ext.BranchRequest{TTL: 2 * time.Hour}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("CheckExtend deny: %v", err)
	}
	_ = r.c.Close()
	if err := pc.ChangePassword(ctx, p, "old", "new"); err == nil {
		t.Error("closed ChangePassword: want error")
	}
	if _, err := r.ResolvePrincipal(ctx, ext.Principal{ID: "user:alice"}); err == nil {
		t.Error("closed ResolvePrincipal: want error")
	}
	if err := r.CheckBranch(ctx, p, ext.BranchRequest{Base: "prod"}); err == nil {
		t.Error("closed CheckBranch: want error")
	}
	if err := ep.CheckExtend(ctx, p, ext.BranchRequest{TTL: 30 * time.Minute}); err == nil {
		t.Error("closed CheckExtend: want error")
	}
}

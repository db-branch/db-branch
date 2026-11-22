package filestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
)

var ctx = context.Background()

func name(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func newStore(t *testing.T) (*filestore.Store, string) {
	path := filepath.Join(t.TempDir(), "state.json")
	return &filestore.Store{Path: path}, path
}

func TestBranch_保存したものを読み戻せて再起動後も残る(t *testing.T) {
	s, path := newStore(t)
	exp := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	b := domain.Branch{Name: name("feature-x"), Base: name("prod"), Status: domain.BranchReady, Port: 40001, Password: "pw", ExpiresAt: &exp}

	if err := s.PutBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
	reopened := &filestore.Store{Path: path}
	got, err := reopened.GetBranch(ctx, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, b) {
		t.Errorf("got %+v, want %+v", got, b)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600 (it holds passwords)", info.Mode().Perm())
	}
}

func TestBranch_一覧は名前順で削除したものは消える(t *testing.T) {
	s, _ := newStore(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		_ = s.PutBranch(ctx, domain.Branch{Name: name(n)})
	}
	if err := s.DeleteBranch(ctx, name("mid")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range list {
		names = append(names, b.Name.String())
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Errorf("names = %v", names)
	}
}

func TestBase_保存と一覧と削除(t *testing.T) {
	s, _ := newStore(t)
	b := domain.Base{Name: name("prod"), Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}
	if err := s.PutBase(ctx, b); err != nil {
		t.Fatal(err)
	}
	_ = s.PutBase(ctx, domain.Base{Name: name("alpha")})

	got, err := s.GetBase(ctx, b.Name)
	if err != nil || !reflect.DeepEqual(got, b) {
		t.Fatalf("GetBase = %+v, %v", got, err)
	}
	list, _ := s.ListBases(ctx)
	if len(list) != 2 || list[0].Name.String() != "alpha" {
		t.Errorf("ListBases = %+v", list)
	}
	if err := s.DeleteBase(ctx, b.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBase(ctx, b.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func Test存在しないものはNotFound(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetBranch: %v", err)
	}
	if err := s.DeleteBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteBranch: %v", err)
	}
	if err := s.DeleteBase(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteBase: %v", err)
	}
}

func Test同時に書き込んでも失われない(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.PutBranch(ctx, domain.Branch{Name: name("b" + string(rune('a'+i)))})
		}(i)
	}
	wg.Wait()
	list, _ := s.ListBranches(ctx)
	if len(list) != 20 {
		t.Errorf("len = %d, want 20", len(list))
	}
}

func Test壊れた状態ファイルや書けない場所はエラーにする(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	s := &filestore.Store{Path: broken}
	if _, err := s.ListBranches(ctx); err == nil {
		t.Error("ListBranches on broken file: want error")
	}
	if _, err := s.ListBases(ctx); err == nil {
		t.Error("ListBases on broken file: want error")
	}
	if _, err := s.GetBase(ctx, name("x")); err == nil {
		t.Error("GetBase on broken file: want error")
	}
	if _, err := s.GetBranch(ctx, name("x")); err == nil {
		t.Error("GetBranch on broken file: want error")
	}
	if err := s.PutBranch(ctx, domain.Branch{Name: name("x")}); err == nil {
		t.Error("PutBranch on broken file: want error")
	}

	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	s = &filestore.Store{Path: filepath.Join(file, "state.json")}
	if err := s.PutBase(ctx, domain.Base{Name: name("x")}); err == nil {
		t.Error("PutBase into a non-directory: want error")
	}
}

func TestInsert_同じ名前があれば上書きせずAlreadyExists(t *testing.T) {
	s, _ := newStore(t)
	if err := s.InsertBranch(ctx, domain.Branch{Name: name("x"), Port: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBranch(ctx, domain.Branch{Name: name("x"), Port: 2}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("second insert: %v", err)
	}
	if b, _ := s.GetBranch(ctx, name("x")); b.Port != 1 {
		t.Errorf("overwritten: %+v", b)
	}
	if err := s.InsertBase(ctx, domain.Base{Name: name("p")}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBase(ctx, domain.Base{Name: name("p")}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("second base insert: %v", err)
	}
}

func TestReplica_記録は上書きされずベースの更新でも残る(t *testing.T) {
	s, _ := newStore(t)
	_ = s.PutBase(ctx, domain.Base{Name: name("p"), Status: domain.BaseReady})
	if err := s.AddReplica(ctx, name("p"), "s1", "n2"); err != nil {
		t.Fatal(err)
	}
	_ = s.PutBase(ctx, domain.Base{Name: name("p"), Status: domain.BaseMaintenance})
	b, _ := s.GetBase(ctx, name("p"))
	if b.Status != domain.BaseMaintenance || !b.HasReplica("s1", "n2") {
		t.Errorf("base = %+v", b)
	}
	if err := s.AddReplica(ctx, name("nope"), "s1", "n2"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
}

func TestRouteUser_保存と一覧と削除(t *testing.T) {
	s, path := newStore(t)
	_ = s.PutRouteUser(ctx, domain.RouteUser{Name: "b", Password: "p"})
	_ = s.PutRouteUser(ctx, domain.RouteUser{Name: "a", Password: "p"})
	list, err := (&filestore.Store{Path: path}).ListRouteUsers(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "a" {
		t.Errorf("list = %+v %v", list, err)
	}
	if err := s.DeleteRouteUser(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRouteUser(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).ListRouteUsers(ctx); err == nil {
		t.Error("broken file: want error")
	}
}

func TestModule_記録と一覧(t *testing.T) {
	s, path := newStore(t)
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "b", Wanted: true})
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "a", Version: "1"})
	list, err := (&filestore.Store{Path: path}).ListModules(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "a" || !list[1].Wanted {
		t.Errorf("list = %+v %v", list, err)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).ListModules(ctx); err == nil {
		t.Error("broken: want error")
	}
}

func TestSettings_範囲ごとに保存する(t *testing.T) {
	s, path := newStore(t)
	if v, err := s.GetSettings(ctx, "core"); err != nil || len(v) != 0 {
		t.Errorf("empty: %v %v", v, err)
	}
	_ = s.PutSettings(ctx, "core", map[string]string{"k": "v"})
	if v, _ := (&filestore.Store{Path: path}).GetSettings(ctx, "core"); v["k"] != "v" {
		t.Errorf("core = %v", v)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).GetSettings(ctx, "core"); err == nil {
		t.Error("broken: want error")
	}
}

func TestTrial_試用の開始は最初の記録が残る(t *testing.T) {
	s, path := newStore(t)
	if at, err := s.TrialStarted(ctx); err != nil || !at.IsZero() {
		t.Errorf("not begun: %s %v", at, err)
	}
	first := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if at, err := s.StartTrial(ctx, first); err != nil || !at.Equal(first) {
		t.Fatalf("begun: %s %v", at, err)
	}
	again := &filestore.Store{Path: path}
	if at, err := again.StartTrial(ctx, first.AddDate(0, 0, 20)); err != nil || !at.Equal(first) {
		t.Errorf("a later start: %s %v", at, err)
	}
	if at, _ := again.TrialStarted(ctx); !at.Equal(first) {
		t.Errorf("recorded: %s", at)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	b := &filestore.Store{Path: broken}
	if _, err := b.TrialStarted(ctx); err == nil {
		t.Error("broken: want error")
	}
	if _, err := b.StartTrial(ctx, first); err == nil {
		t.Error("broken: want error")
	}
}

func TestClusterID_クラスタidは最初の記録が残り再起動後も同じ(t *testing.T) {
	s, path := newStore(t)
	if id := s.ClusterID(ctx); id != "" {
		t.Errorf("not recorded: %q", id)
	}
	first, second := strings.Repeat("0a", 16), strings.Repeat("1b", 16)
	if id, err := s.InitClusterID(ctx, first); err != nil || id != first {
		t.Fatalf("first: %q %v", id, err)
	}
	again := &filestore.Store{Path: path}
	if id, err := again.InitClusterID(ctx, second); err != nil || id != first {
		t.Errorf("a later id: %q %v", id, err)
	}
	if id := again.ClusterID(ctx); id != first {
		t.Errorf("recorded: %q", id)
	}
	if _, err := s.InitClusterID(ctx, "not-an-id"); err == nil {
		t.Error("an invalid id is refused")
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	b := &filestore.Store{Path: broken}
	if id := b.ClusterID(ctx); id != "" {
		t.Errorf("broken: %q", id)
	}
	if _, err := b.InitClusterID(ctx, first); err == nil {
		t.Error("broken: want error")
	}
}

func TestToken_保存と一覧と削除(t *testing.T) {
	s, path := newStore(t)
	t0 := time.Unix(100, 0)
	_ = s.PutToken(ctx, domain.APIToken{ID: "b", CreatedAt: t0.Add(time.Second)})
	_ = s.PutToken(ctx, domain.APIToken{ID: "a", CreatedAt: t0})
	if list, _ := (&filestore.Store{Path: path}).ListTokens(ctx); len(list) != 2 || list[0].ID != "a" {
		t.Errorf("list = %+v", list)
	}
	if err := s.DeleteToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteToken(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).ListTokens(ctx); err == nil {
		t.Error("broken: want error")
	}
}

func TestModuleData_モジュールごとに保存し消せる(t *testing.T) {
	s, path := newStore(t)
	_ = s.PutModuleData(ctx, "auth", "user/a", []byte("1"))
	_ = s.PutModuleData(ctx, "auth", "user/b", []byte("2"))
	if v, _ := (&filestore.Store{Path: path}).ListModuleData(ctx, "auth"); len(v) != 2 || string(v["user/b"]) != "2" {
		t.Errorf("auth = %v", v)
	}
	_ = s.DeleteModuleData(ctx, "auth", "user/a")
	_ = s.DeleteModuleData(ctx, "auth", "user/b")
	if v, _ := s.ListModuleData(ctx, "auth"); len(v) != 0 {
		t.Errorf("after delete = %v", v)
	}
	broken := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).ListModuleData(ctx, "auth"); err == nil {
		t.Error("broken: want error")
	}
}

func sampleRefresh() domain.BaseRefresh {
	return domain.BaseRefresh{
		Base:      name("prod"),
		Source:    domain.RefreshSource{Host: "db", Port: 5432, User: "u", Password: "pw", Databases: []string{"app"}},
		Every:     24 * time.Hour,
		UpdatedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
	}
}

func TestBaseRefresh_保存して読み戻し実行結果を足し削除できる(t *testing.T) {
	s, path := newStore(t)
	if _, err := s.GetBaseRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if err := s.SetBaseRefresh(ctx, sampleRefresh()); err != nil {
		t.Fatal(err)
	}
	run := domain.RefreshRun{StartedAt: time.Unix(100, 0).UTC(), FinishedAt: time.Unix(160, 0).UTC(), OK: true}
	if err := s.RecordBaseRefreshRun(ctx, name("prod"), run); err != nil {
		t.Fatal(err)
	}
	// A new setting keeps the last run.
	changed := sampleRefresh()
	changed.Every = 48 * time.Hour
	if err := s.SetBaseRefresh(ctx, changed); err != nil {
		t.Fatal(err)
	}
	got, err := (&filestore.Store{Path: path}).GetBaseRefresh(ctx, name("prod"))
	if err != nil || got.Every != 48*time.Hour || got.LastRun == nil || !got.LastRun.OK {
		t.Fatalf("got %+v, %v", got, err)
	}
	list, _ := s.ListBaseRefreshes(ctx)
	if len(list) != 1 {
		t.Errorf("list = %v", list)
	}
	if err := s.ClearBaseRefresh(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearBaseRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("clear twice = %v", err)
	}
	if err := s.RecordBaseRefreshRun(ctx, name("prod"), run); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record without schedule = %v", err)
	}
}

func TestDeleteBase_ベースを消すと更新スケジュールも消える(t *testing.T) {
	s, _ := newStore(t)
	_ = s.PutBase(ctx, domain.Base{Name: name("prod")})
	_ = s.SetBaseRefresh(ctx, sampleRefresh())
	if err := s.DeleteBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBaseRefreshes(ctx); len(list) != 0 {
		t.Errorf("list = %v", list)
	}
}

func TestStore_更新の確認とロールアウトを保存する(t *testing.T) {
	s, _ := newStore(t)
	if c, err := s.GetUpdateCheck(ctx); err != nil || !c.At.IsZero() {
		t.Errorf("no check yet: %+v %v", c, err)
	}
	if _, err := s.GetRollout(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no rollout yet: %v", err)
	}
	if err := s.PutUpdateCheck(ctx, domain.UpdateCheck{By: "n", Packages: map[string][]domain.PackageVersion{"db-branch": {{Version: "1.1.0"}}}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetUpdateCheck(ctx); c.By != "n" || c.Packages["db-branch"][0].Version != "1.1.0" {
		t.Errorf("check = %+v", c)
	}
	at := time.Unix(1, 0).UTC()
	r := domain.NewRollout("r1", "1.1.0", []string{"n"}, map[string]string{"n": "1.0.0"}, at)
	if err := s.StartRollout(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRollout(ctx, r); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second start: %v", err)
	}
	for _, ev := range []domain.RolloutEvent{
		{Rollout: "r1", Node: "n", State: domain.NodeUpdating, At: at},
		{Rollout: "r1", Node: "n", State: domain.NodeDone, At: at},
		{Rollout: "r1", Status: domain.RolloutDone, At: at},
	} {
		if err := s.RecordRollout(ctx, ev); err != nil {
			t.Fatalf("%+v: %v", ev, err)
		}
	}
	if got, _ := s.GetRollout(ctx); got.Status != domain.RolloutDone || got.Nodes["n"].State != domain.NodeDone {
		t.Errorf("rollout = %+v", got)
	}
	if err := s.RecordRollout(ctx, domain.RolloutEvent{Rollout: "other", Status: domain.RolloutDone}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("end of another rollout: %v", err)
	}
	if err := s.RecordRollout(ctx, domain.RolloutEvent{Rollout: "r1", Node: "n", State: domain.NodeUpdating}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("bad transition: %v", err)
	}
	empty, _ := newStore(t)
	if err := empty.RecordRollout(ctx, domain.RolloutEvent{Rollout: "r1"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("event without a rollout: %v", err)
	}
}

func TestReplica_レプリカ先の設定と記録の削除はベースの更新でも残る(t *testing.T) {
	s, _ := newStore(t)
	_ = s.PutBase(ctx, domain.Base{Name: name("p")})
	_ = s.AddReplica(ctx, name("p"), "s1", "n2")
	if err := s.SetBaseReplicaNodes(ctx, name("p"), &[]string{"n3"}); err != nil {
		t.Fatal(err)
	}
	_ = s.PutBase(ctx, domain.Base{Name: name("p"), Status: domain.BaseReady})
	b, _ := s.GetBase(ctx, name("p"))
	if b.ReplicaNodes == nil || (*b.ReplicaNodes)[0] != "n3" {
		t.Errorf("setting lost: %+v", b)
	}
	if err := s.RemoveReplica(ctx, name("p"), "n2"); err != nil {
		t.Fatal(err)
	}
	if b, _ = s.GetBase(ctx, name("p")); b.HoldsAny("n2") {
		t.Errorf("replica kept: %+v", b)
	}
	if !errors.Is(s.RemoveReplica(ctx, name("nope"), "n2"), domain.ErrNotFound) || !errors.Is(s.SetBaseReplicaNodes(ctx, name("nope"), nil), domain.ErrNotFound) {
		t.Error("missing base: want ErrNotFound")
	}
}

func TestAnonymous_匿名の上限はベースの更新でも残り継承に戻せる(t *testing.T) {
	s, _ := newStore(t)
	_ = s.PutBase(ctx, domain.Base{Name: name("p")})
	three := 3
	if err := s.SetBaseAnonymous(ctx, name("p"), domain.AnonymousLimits{MaxBranches: &three, MaxTTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	_ = s.PutBase(ctx, domain.Base{Name: name("p"), Status: domain.BaseReady})
	b, _ := s.GetBase(ctx, name("p"))
	if b.Anonymous.MaxBranches == nil || *b.Anonymous.MaxBranches != 3 || b.Anonymous.MaxTTL != time.Hour || b.Status != domain.BaseReady {
		t.Errorf("limits lost: %+v", b)
	}
	_ = s.SetBaseAnonymous(ctx, name("p"), domain.AnonymousLimits{})
	if b, _ = s.GetBase(ctx, name("p")); !b.Anonymous.Inherits() {
		t.Errorf("want inherit: %+v", b.Anonymous)
	}
	if !errors.Is(s.SetBaseAnonymous(ctx, name("nope"), domain.AnonymousLimits{}), domain.ErrNotFound) {
		t.Error("missing base: want ErrNotFound")
	}
}

func TestPutBranch_記録の有無は作成時に決まり後から書き換えても変わらない(t *testing.T) {
	s, _ := newStore(t)
	for _, tc := range []struct {
		name         string
		first, later bool
	}{{"rec", true, false}, {"plain", false, true}} {
		_ = s.InsertBranch(ctx, domain.Branch{Name: name(tc.name), Record: tc.first})
		if err := s.PutBranch(ctx, domain.Branch{Name: name(tc.name), Status: domain.BranchReady, Record: tc.later}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetBranch(ctx, name(tc.name))
		if got.Record != tc.first || got.Status != domain.BranchReady {
			t.Errorf("%s: %+v, want record=%v and the other fields replaced", tc.name, got, tc.first)
		}
	}
	// A branch the store does not know yet takes the record as given.
	_ = s.PutBranch(ctx, domain.Branch{Name: name("new"), Record: true})
	if got, _ := s.GetBranch(ctx, name("new")); !got.Record {
		t.Error("a new branch should keep record")
	}
}

func TestDeleteModule_記録を消せ存在しない名前でも冪等に成功する(t *testing.T) {
	s, path := newStore(t)
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "1.0", Wanted: false})
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "proxysql", Version: "1.0", Wanted: true})

	if err := s.DeleteModule(ctx, "audit"); err != nil {
		t.Fatal(err)
	}
	list, err := (&filestore.Store{Path: path}).ListModules(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "proxysql" {
		t.Fatalf("after delete = %+v, %v", list, err)
	}
	// Idempotent: deleting a missing module succeeds and keeps remaining records.
	if err := s.DeleteModule(ctx, "audit"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
	if err := s.DeleteModule(ctx, "never-existed"); err != nil {
		t.Errorf("delete unknown: %v", err)
	}
	if list, _ := s.ListModules(ctx); len(list) != 1 || list[0].Name != "proxysql" {
		t.Errorf("after idempotent delete = %+v", list)
	}
}

func TestSnapshot_全項目を欠落なく取得できる(t *testing.T) {
	s, _ := newStore(t)
	_ = s.PutBase(ctx, domain.Base{Name: name("prod"), Node: "n1", Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4"}, Status: domain.BaseReady})
	_ = s.PutBase(ctx, domain.Base{Name: name("alpha"), Node: "n1", Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4"}, Status: domain.BaseReady})
	two := 2
	_ = s.SetBaseAnonymous(ctx, name("prod"), domain.AnonymousLimits{MaxBranches: &two, MaxTTL: time.Hour})
	_ = s.SetBaseReplicaNodes(ctx, name("prod"), &[]string{"n2"})
	_ = s.AddReplica(ctx, name("prod"), "s1", "n2")
	_ = s.PutBranch(ctx, domain.Branch{Name: name("feat"), Base: name("prod"), Status: domain.BranchReady, Record: true})
	_ = s.PutBranch(ctx, domain.Branch{Name: name("dev"), Base: name("prod"), Status: domain.BranchReady})
	_ = s.PutRouteUser(ctx, domain.RouteUser{Name: "web", DefaultBranch: name("feat")})
	_ = s.PutRouteUser(ctx, domain.RouteUser{Name: "app", DefaultBranch: name("feat")})
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "proxysql", Version: "1.0.0", Wanted: true})
	_ = s.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "1.0.0", Wanted: true})
	_ = s.PutSettings(ctx, "core", map[string]string{"branch.memory_mb": "512"})
	_ = s.PutSettings(ctx, "audit", map[string]string{"retain_days": "30"})
	_ = s.PutSettings(ctx, "pref:admin", map[string]string{"language": "ja"})
	_, _ = s.StartTrial(ctx, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	_ = s.PutToken(ctx, domain.APIToken{ID: "t2", Name: "ci2", Hash: "def", Scope: domain.ScopeBranches, CreatedAt: time.Unix(200, 0).UTC()})
	_ = s.PutToken(ctx, domain.APIToken{ID: "t1", Name: "ci1", Hash: "abc", Scope: domain.ScopeBranches, CreatedAt: time.Unix(100, 0).UTC()})
	_ = s.PutModuleData(ctx, "auth", "user/u1", []byte(`{"name":"alice"}`))
	_ = s.SetBaseRefresh(ctx, domain.BaseRefresh{Base: name("prod"), Every: 24 * time.Hour})
	_ = s.SetBaseRefresh(ctx, domain.BaseRefresh{Base: name("alpha"), Every: 12 * time.Hour})
	_ = s.RecordBaseRefreshRun(ctx, name("prod"), domain.RefreshRun{StartedAt: time.Unix(200, 0).UTC(), FinishedAt: time.Unix(210, 0).UTC(), OK: true})
	_ = s.PutUpdateCheck(ctx, domain.UpdateCheck{At: time.Unix(300, 0).UTC(), By: "n1"})
	ro := domain.NewRollout("r1", "1.0.0", []string{"n1"}, map[string]string{"n1": "0.2.7"}, time.Unix(400, 0).UTC())
	_ = s.StartRollout(ctx, ro)
	_, _ = s.InitClusterID(ctx, "0123456789abcdef0123456789abcdef")

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Bases) != 2 || snap.Bases[0].Name != name("alpha") || snap.Bases[1].Name != name("prod") {
		t.Fatalf("bases = %+v", snap.Bases)
	}
	prod := snap.Bases[1]
	if prod.Anonymous.MaxBranches == nil || *prod.Anonymous.MaxBranches != 2 || prod.Anonymous.MaxTTL != time.Hour {
		t.Errorf("prod.Anonymous = %+v", prod.Anonymous)
	}
	if prod.ReplicaNodes == nil || len(*prod.ReplicaNodes) != 1 || (*prod.ReplicaNodes)[0] != "n2" || !prod.HasReplica("s1", "n2") {
		t.Errorf("base replicas = %+v, replica_nodes = %+v", prod.Replicas, prod.ReplicaNodes)
	}
	if len(snap.Branches) != 2 || snap.Branches[0].Name != name("dev") || snap.Branches[1].Name != name("feat") || !snap.Branches[1].Record {
		t.Errorf("branches = %+v", snap.Branches)
	}
	if len(snap.RouteUsers) != 2 || snap.RouteUsers[0].Name != "app" || snap.RouteUsers[1].Name != "web" {
		t.Errorf("route users = %+v", snap.RouteUsers)
	}
	if len(snap.Modules) != 2 || snap.Modules[0].Name != "audit" || snap.Modules[1].Name != "proxysql" {
		t.Errorf("modules = %+v", snap.Modules)
	}
	if snap.Settings["core"]["branch.memory_mb"] != "512" || snap.Settings["audit"]["retain_days"] != "30" || snap.Settings["pref:admin"]["language"] != "ja" || snap.Settings[domain.TrialScope][domain.TrialKey] == "" {
		t.Errorf("settings = %+v", snap.Settings)
	}
	if len(snap.Tokens) != 2 || snap.Tokens[0].ID != "t1" || snap.Tokens[1].ID != "t2" {
		t.Errorf("tokens = %+v", snap.Tokens)
	}
	if string(snap.ModuleData["auth"]["user/u1"]) != `{"name":"alice"}` {
		t.Errorf("module data = %+v", snap.ModuleData)
	}
	if len(snap.BaseRefreshes) != 2 || snap.BaseRefreshes[0].Base != name("alpha") || snap.BaseRefreshes[1].LastRun == nil || !snap.BaseRefreshes[1].LastRun.OK {
		t.Errorf("refreshes = %+v", snap.BaseRefreshes)
	}
	if snap.Update == nil || snap.Update.At.Unix() != 300 {
		t.Errorf("update = %+v", snap.Update)
	}
	if snap.Rollout == nil || snap.Rollout.ID != "r1" {
		t.Errorf("rollout = %+v", snap.Rollout)
	}
	if snap.ClusterID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("cluster id = %q", snap.ClusterID)
	}

	broken := filepath.Join(t.TempDir(), "broken.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (&filestore.Store{Path: broken}).Snapshot(); err == nil {
		t.Error("corrupt state file: want error")
	}
}

func TestMigrateBranchOwner_未移行ブランチの所有者IDと表示名を原子的に移行し冪等に保つ(t *testing.T) {
	s, _ := newStore(t)
	exp := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	_ = s.PutBranch(ctx, domain.Branch{Name: name("legacy"), Base: name("prod"), Status: domain.BranchReady, Owner: "ci", ExpiresAt: &exp})
	_ = s.PutBranch(ctx, domain.Branch{Name: name("noowner"), Base: name("prod"), Status: domain.BranchReady})

	if err := s.MigrateBranchOwner(ctx, name("legacy"), "token:abc", "ci"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBranch(ctx, name("legacy"))
	if got.Owner != "token:abc" || got.OwnerName != "ci" || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("after migrate = %+v", got)
	}
	// Already migrated: second call is a no-op and must not overwrite.
	if err := s.MigrateBranchOwner(ctx, name("legacy"), "token:other", "other"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetBranch(ctx, name("legacy")); got.Owner != "token:abc" || got.OwnerName != "ci" {
		t.Errorf("overwritten migrated branch = %+v", got)
	}
	// Empty owner and missing branch are no-ops.
	if err := s.MigrateBranchOwner(ctx, name("noowner"), "token:x", "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetBranch(ctx, name("noowner")); got.Owner != "" || got.OwnerName != "" {
		t.Errorf("empty owner modified = %+v", got)
	}
	if err := s.MigrateBranchOwner(ctx, name("missing"), "token:x", "x"); err != nil {
		t.Fatal(err)
	}
	// PutBranch without OwnerName for the same Owner preserves OwnerName.
	_ = s.PutBranch(ctx, domain.Branch{Name: name("legacy"), Base: name("prod"), Status: domain.BranchPaused, Owner: "token:abc"})
	if got, _ = s.GetBranch(ctx, name("legacy")); got.Status != domain.BranchPaused || got.OwnerName != "ci" {
		t.Errorf("PutBranch dropped OwnerName = %+v", got)
	}
}

package rtserver_test

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/rtserver"
)

func TestNewSystem_実際のsystemdとファイルシステムで動く構成を作る(t *testing.T) {
	e := newEnv(t)
	srv := rtserver.NewSystem(domain.EngineSpec{Engine: "fakeengine"}, e.prov, "db-branch-rt-fakeengine", "/var/lib/db-branch", "10.0.0.1")
	if srv.User != "db-branch-rt-fakeengine" || srv.StateRoot != "/var/lib/db-branch/instances" ||
		srv.RuntimeRoot != "/var/lib/db-branch/runtimes" || srv.Bind != "10.0.0.1" || srv.Run == nil ||
		srv.StartTimeout != 2*time.Minute || srv.PollInterval <= 0 {
		t.Fatalf("%+v", srv)
	}
}

func TestNewSystem_ディレクトリを作り自分のユーザーには渡せる(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	srv := rtserver.NewSystem(domain.EngineSpec{}, nil, cur.Username, "/x", "")
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := srv.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("%v", err)
	}
	if err := srv.Chown(cur.Username, dir); err != nil {
		t.Fatal(err)
	}
	if err := srv.Chown("db-branch-rt-no-such-user-xyz", dir); err == nil {
		t.Fatal("an unknown user must be an error")
	}
}

func TestNewSystem_親ディレクトリは誰でも通れ末端だけを絞る(t *testing.T) {
	srv := rtserver.NewSystem(domain.EngineSpec{}, nil, "x", "/x", "")
	base := t.TempDir()
	dir := filepath.Join(base, "run", "engine", "id")
	if err := srv.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	// The module runs as its own user and has to walk down to its directory.
	for _, d := range []string{filepath.Join(base, "run"), filepath.Join(base, "run", "engine")} {
		if st, _ := os.Stat(d); st.Mode().Perm()&0o005 != 0o005 {
			t.Errorf("%s is %v: others must be able to enter it", d, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o750 {
		t.Errorf("the instance's own directory is %v, want 0750", st.Mode().Perm())
	}
	// Making it again is fine.
	if err := srv.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestNewSystem_ダンプとその親ディレクトリがモジュール所有かを確かめられる(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	srv := rtserver.NewSystem(domain.EngineSpec{}, nil, cur.Username, "/x", "")
	dir := t.TempDir()
	f := filepath.Join(dir, "d")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !srv.OwnedBy(cur.Username, f) {
		t.Fatal("a file and its directory owned by the user must count as theirs")
	}
	if srv.OwnedBy("db-branch-rt-no-such-user-xyz", f) {
		t.Fatal("an unknown user owns nothing")
	}
	if srv.OwnedBy(cur.Username, filepath.Join(dir, "missing")) {
		t.Fatal("a missing file is not owned")
	}
	if cur.Uid != "0" && srv.OwnedBy("root", f) {
		t.Fatal("a file of another user must not count as root's")
	}
}

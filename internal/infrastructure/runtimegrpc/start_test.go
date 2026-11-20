package runtimegrpc_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
)

func manifest(user string) domain.RuntimeManifest {
	return domain.RuntimeManifest{
		Engine: "fakeengine", DisplayName: "Fake", RunAsUser: user, Binary: "/usr/lib/db-branch/runtimes/fakeengine/db-branch-rt-fakeengine",
		Protocols: []domain.ManifestPort{{Name: "sql", Primary: true}},
	}
}

func TestCommand_専用ユーザーの資格で起動し環境を引き継がない(t *testing.T) {
	t.Setenv("DB_BRANCH_SECRET", "s3cret")
	var asked string
	cmd, err := runtimegrpc.Command(manifest(""), func(name string) (uint32, uint32, error) { asked = name; return 991, 992, nil })
	if err != nil {
		t.Fatal(err)
	}
	if asked != "db-branch-rt-fakeengine" {
		t.Fatalf("looked up %q", asked)
	}
	if cmd.Path != "/usr/lib/db-branch/runtimes/fakeengine/db-branch-rt-fakeengine" || cmd.Dir != "/" {
		t.Fatalf("path/dir = %q %q", cmd.Path, cmd.Dir)
	}
	cred := cmd.SysProcAttr.Credential
	if cred == nil || cred.Uid != 991 || cred.Gid != 992 || cred.NoSetGroups {
		t.Fatalf("credential = %+v", cred)
	}
	for _, e := range cmd.Env {
		if strings.Contains(e, "s3cret") || strings.HasPrefix(e, "DB_BRANCH_SECRET") {
			t.Fatalf("the agent's environment leaked: %v", cmd.Env)
		}
	}
}

func TestCommand_マニフェストが指定したユーザーを使う(t *testing.T) {
	var asked string
	_, err := runtimegrpc.Command(manifest("db-branch-rt-fakeengine"), func(name string) (uint32, uint32, error) { asked = name; return 1, 2, nil })
	if err != nil || asked != "db-branch-rt-fakeengine" {
		t.Fatalf("%q %v", asked, err)
	}
}

func TestCommand_特権ユーザーでは起動しない(t *testing.T) {
	for _, uid := range []uint32{0} {
		_, err := runtimegrpc.Command(manifest(""), func(string) (uint32, uint32, error) { return uid, 5, nil })
		if err == nil {
			t.Fatalf("uid %d must be refused", uid)
		}
	}
	_, err := runtimegrpc.Command(manifest(""), func(string) (uint32, uint32, error) { return 5, 0, nil })
	if err == nil {
		t.Fatal("gid 0 must be refused")
	}
}

func TestCommand_ユーザーが無ければ起動しない(t *testing.T) {
	_, err := runtimegrpc.Command(manifest(""), func(string) (uint32, uint32, error) { return 0, 0, errors.New("unknown user") })
	if err == nil || !strings.Contains(err.Error(), "db-branch-rt-fakeengine") {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_Describeがマニフェストと違うモジュールは止めて拒否する(t *testing.T) {
	m := manifest("")
	m.DisplayName = "Not what the module says" // the module says "Fake"
	_, _, err := runtimegrpc.Start(moduleCmd(), m)
	if !errors.Is(err, domain.ErrInvalidManifest) {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_マニフェストと一致するモジュールを受け入れる(t *testing.T) {
	p, stop, err := runtimegrpc.Start(moduleCmd(), manifest(""))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := p.Health(context.Background(), domain.InstanceSpec{ID: "x"}); err == nil {
		t.Fatal("the fake does not implement Health: expected an error")
	}
}

func TestStart_Describeに失敗するモジュールは止めて拒否する(t *testing.T) {
	cmd := moduleCmd()
	cmd.Env = append(cmd.Env, "DBB_TEST_MODULE=broken") // the last value wins
	_, _, err := runtimegrpc.Start(cmd, manifest(""))
	if err == nil || !strings.Contains(err.Error(), "cannot describe") {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_モジュールでないバイナリは拒否する(t *testing.T) {
	_, _, err := runtimegrpc.Start(exec.Command("/bin/true"), manifest(""))
	if err == nil || !strings.Contains(err.Error(), "fakeengine") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemUser_システムのユーザーを引ける(t *testing.T) {
	uid, gid, err := runtimegrpc.SystemUser("root")
	if err != nil || uid != 0 || gid != 0 {
		t.Fatalf("%d %d %v", uid, gid, err)
	}
	if _, _, err := runtimegrpc.SystemUser("db-branch-rt-no-such-user-xyz"); err == nil {
		t.Fatal("an unknown user must be an error")
	}
}

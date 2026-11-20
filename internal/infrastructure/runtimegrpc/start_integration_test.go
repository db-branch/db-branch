//go:build integration

package runtimegrpc_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
)

// Needs root on a Linux host (run with sudo on the dev VM). It makes the OS
// user db-branch-rt-itfake and removes both user and files afterwards.
func TestStart_実在の専用ユーザーで起動し環境を引き継がない(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	const name = "db-branch-rt-itfake"
	if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name).CombinedOutput(); err != nil {
		t.Fatalf("useradd: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("userdel", name).Run() })
	dir, err := os.MkdirTemp("", "itfake")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { // the module's user must reach its binary
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "db-branch-rt-itfake")
	copyFile(t, os.Args[0], bin)
	t.Setenv("DB_BRANCH_SECRET", "s3cret")

	m := manifest("")
	m.Binary = bin
	m.DisplayName = "Fake"
	// The fake module answers Describe with engine "fakeengine".
	cmd, err := runtimegrpc.Command(m, runtimegrpc.SystemUser)
	if err == nil {
		t.Fatal("the user for engine fakeengine does not exist: expected an error")
	}
	m.RunAsUser = name
	cmd, err = runtimegrpc.Command(m, runtimegrpc.SystemUser)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "DBB_TEST_MODULE=1")
	p, stop, err := runtimegrpc.Start(cmd, m)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	a, err := p.AdminAccess(context.Background(), domain.InstanceSpec{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if a.User == "0" || a.Host == "0" {
		t.Fatalf("the module runs as root: uid %s gid %s", a.User, a.Host)
	}
	if _, err := strconv.Atoi(a.User); err != nil {
		t.Fatal(err)
	}
	if a.Env["DB_BRANCH_SECRET"] != "" {
		t.Fatalf("the agent's environment leaked: %v", a.Env)
	}
	t.Logf("module ran as uid %s gid %s", a.User, a.Host)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

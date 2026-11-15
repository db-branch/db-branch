package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
)

func TestUseEvaluationPassword_adminで入れて初回に変更を求める(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	n := Node{InitialPasswordFile: filepath.Join(t.TempDir(), "initial-admin-password")}
	if err := os.WriteFile(n.InitialPasswordFile, []byte("random-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := authService(c)
	if err := svc.SetPassword("random-secret"); err != nil {
		t.Fatal(err)
	}

	if err := n.UseEvaluationPassword(c); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login("admin", "random-secret"); err == nil {
		t.Error("the random password must stop working")
	}
	sess, err := svc.Login("admin", "admin")
	if err != nil || !sess.MustChange {
		t.Fatalf("login with admin: %+v %v", sess, err)
	}
	if _, err := os.Stat(n.InitialPasswordFile); !os.IsNotExist(err) {
		t.Errorf("the saved initial password must be removed: %v", err)
	}
}

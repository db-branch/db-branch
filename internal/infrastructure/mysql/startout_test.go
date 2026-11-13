package mysql_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestStart_mysqldがログを書く前に落ちたらジャーナルの出力を添える(t *testing.T) {
	f := newFixture(t)
	f.sys.crashes = true
	f.sys.pingFails = 1 << 30
	f.sys.outputs = map[string]string{"journalctl": "sh: 1: exec: /rt/bin/mysqld: Permission denied\n"}
	if err := f.srv.Start(ctx, f.spec); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("start: %v", err)
	}
	got, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{})
	if err == nil || !strings.Contains(got.Excerpt, "Permission denied") {
		t.Errorf("upgrade: %+v %v", got, err)
	}
}

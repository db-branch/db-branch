package domain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestModule_名前とパッケージ(t *testing.T) {
	if (domain.ModuleSpec{Name: "proxysql"}).Package() != "db-branch-proxysql" {
		t.Error("package name")
	}
	for _, bad := range []string{"", "Proxy", "-x", "a b", "x;rm", "averyveryveryverylongmodulenameabc"} {
		if !errors.Is(domain.ValidModuleName(bad), domain.ErrInvalidModule) {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := domain.ValidModuleName("audit-s3"); err != nil {
		t.Error(err)
	}
}

func TestPendingModules_欲しいのに読み込まれていないものだけ(t *testing.T) {
	specs := []domain.ModuleSpec{{Name: "proxysql", Wanted: true}, {Name: "auth", Wanted: true}, {Name: "old", Wanted: false}}
	got := domain.PendingModules(specs, []string{"proxysql", "audit-file"})
	if len(got) != 1 || got[0] != "auth" {
		t.Errorf("pending = %v", got)
	}
}

func TestProtocolModuleForget_プロトコル22が予約されている(t *testing.T) {
	if domain.ProtocolModuleForget != 22 {
		t.Errorf("ProtocolModuleForget = %d, want 22", domain.ProtocolModuleForget)
	}
	var _ interface {
		DeleteModule(context.Context, string) error
	} = (domain.ModuleStore)(nil)
}

package routing_test

import (
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/routing"
)

func TestPlan_フォーク中のブランチはルーティング対象から外れる(t *testing.T) {
	hosts := map[string]string{"n1": "192.0.2.10"}
	branches := []domain.Branch{
		branch("parent", "n1", 40001, domain.BranchForking),
		branch("other", "n1", 40002, domain.BranchReady),
	}
	c := routing.Plan(branches, hosts, nil).MySQL
	if len(c.Servers) != 1 || c.Servers[0].Port != 40002 {
		t.Fatalf("servers = %+v, want only ready branch", c.Servers)
	}
	if len(c.Users) != 1 || c.Users[0].Name != "other" {
		t.Fatalf("users = %+v, want only ready branch", c.Users)
	}
}

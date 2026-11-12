package domain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestHostgroups_名前から決まり他のブランチが増減しても変わらない(t *testing.T) {
	a := domain.Hostgroups([]domain.Name{name("feature-x"), name("pr-1")})
	b := domain.Hostgroups([]domain.Name{name("pr-1"), name("other"), name("feature-x")})
	if a[name("feature-x")] != b[name("feature-x")] || a[name("pr-1")] != b[name("pr-1")] {
		t.Errorf("ids moved: %v %v", a, b)
	}
	if a[name("feature-x")] < 1000 || a[name("feature-x")] == a[name("pr-1")] {
		t.Errorf("ids = %v", a)
	}
}

func TestHostgroups_衝突しても重ならない(t *testing.T) {
	// "plumless" and "buckeroo" share a CRC-32.
	got := domain.Hostgroups([]domain.Name{name("plumless"), name("buckeroo")})
	if got[name("plumless")] == got[name("buckeroo")] {
		t.Errorf("collision not resolved: %v", got)
	}
}

func TestRouteUser_名前とパスワードが要りブランチのユーザーと重ならない(t *testing.T) {
	branches := []domain.Branch{{Name: name("feature-x"), User: "feature-x"}}
	for _, u := range []domain.RouteUser{
		{Name: "", Password: "p"}, {Name: "app"}, {Name: "feature-x", Password: "p"},
		{Name: "a-very-long-user-name-over-32-chars", Password: "p"},
	} {
		if err := u.Validate(branches); !errors.Is(err, domain.ErrInvalidRouteUser) {
			t.Errorf("%+v: %v", u, err)
		}
	}
	if err := (domain.RouteUser{Name: "app", Password: "p"}).Validate(branches); err != nil {
		t.Error(err)
	}
}

func name(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

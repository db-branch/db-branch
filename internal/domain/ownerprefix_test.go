package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestOwnerPrefix_所有者名をブランチ名に使える形へ決まった規則で直す(t *testing.T) {
	cases := map[string]string{
		"alice":                "alice",
		"Alice":                "alice",
		"alice@example.com":    "alice",
		"Taro.Yamada@Corp.jp":  "taro-yamada",
		"first_last":           "first-last",
		"a..b--c":              "a-b-c",
		"-bob-":                "bob",
		"42user":               "u42user",
		"123":                  "u123",
		"":                     "u",
		"@example.com":         "u",
		"日本語":                  "u",
		"ci-deploy":            "ci-deploy",
		"user+tag@example.com": "user-tag",
	}
	for owner, want := range cases {
		if got := domain.OwnerPrefix(owner); got != want {
			t.Errorf("OwnerPrefix(%q) = %q, want %q", owner, got, want)
		}
		if _, err := domain.ParseName(domain.OwnerPrefix(owner)); err != nil {
			t.Errorf("OwnerPrefix(%q) is not a valid name: %v", owner, err)
		}
	}
}

func TestPrefixedName_入力した名前の前に所有者を付ける(t *testing.T) {
	got, err := domain.PrefixedName("alice", "test")
	if err != nil || got != "alice-test" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPrefixedName_すでに付いていれば重ねない(t *testing.T) {
	got, err := domain.PrefixedName("alice", "alice-test")
	if err != nil || got != "alice-test" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPrefixedName_所有者名で始まるだけの名前には付ける(t *testing.T) {
	got, err := domain.PrefixedName("alice", "alicetest")
	if err != nil || got != "alice-alicetest" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPrefixedName_ちょうど上限まで受け付ける(t *testing.T) {
	typed := strings.Repeat("a", 32-len("alice-"))
	got, err := domain.PrefixedName("alice", typed)
	if err != nil || len(got) != 32 {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPrefixedName_長すぎれば入力できる最大の長さを伝えて拒否する(t *testing.T) {
	_, err := domain.PrefixedName("alice", strings.Repeat("a", 27))
	if !errors.Is(err, domain.ErrInvalidName) || !strings.Contains(err.Error(), "at most 26 characters") || !strings.Contains(err.Error(), "alice-") {
		t.Errorf("err = %v", err)
	}
	_, err = domain.PrefixedName("alice", "alice-"+strings.Repeat("a", 27))
	if !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("already prefixed too long: %v", err)
	}
}

func TestPrefixedName_所有者名が長すぎて入力する余地がなければ拒否する(t *testing.T) {
	_, err := domain.PrefixedName(strings.Repeat("o", 40), "x")
	if !errors.Is(err, domain.ErrInvalidName) || !strings.Contains(err.Error(), "too long") {
		t.Errorf("err = %v", err)
	}
}

func TestPrefixedName_入力部分が名前の規則に合わなければ拒否する(t *testing.T) {
	for _, typed := range []string{"", "Test", "1abc", "a--b", "a_b"} {
		if _, err := domain.PrefixedName("alice", typed); !errors.Is(err, domain.ErrInvalidName) {
			t.Errorf("typed %q: err = %v", typed, err)
		}
	}
}

package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestParseName_英小文字と数字とハイフンの名前を受け付ける(t *testing.T) {
	for _, s := range []string{"a", "feature-x", "pr-1234", "a1b2", strings.Repeat("a", 32)} {
		n, err := domain.ParseName(s)
		if err != nil {
			t.Errorf("ParseName(%q) error: %v", s, err)
			continue
		}
		if n.String() != s {
			t.Errorf("String() = %q, want %q", n.String(), s)
		}
	}
}

func TestParseName_ZFSやSQLで危険になりうる名前を拒否する(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"too long":        strings.Repeat("a", 33),
		"uppercase":       "Feature",
		"leading digit":   "1abc",
		"leading hyphen":  "-rf",
		"trailing hyphen": "abc-",
		"double hyphen":   "a--b",
		"slash":           "a/b",
		"at sign":         "a@snap",
		"dot":             "a.b",
		"underscore":      "a_b",
		"space":           "a b",
		"quote":           "a'b",
		"non-ascii":       "ブランチ",
	}
	for label, s := range cases {
		if _, err := domain.ParseName(s); !errors.Is(err, domain.ErrInvalidName) {
			t.Errorf("%s: ParseName(%q) error = %v, want ErrInvalidName", label, s, err)
		}
	}
}

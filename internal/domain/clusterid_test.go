package domain_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestValidClusterID_小文字16進32桁だけを受け入れる(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	if !domain.ValidClusterID(good) {
		t.Errorf("%q は有効", good)
	}
	for _, bad := range []string{
		"", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0",
		"0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef0123456789abcdeg",
		" 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef\n",
	} {
		if domain.ValidClusterID(bad) {
			t.Errorf("%q は無効", bad)
		}
	}
}

func TestNewClusterID_乱数16バイトから32桁の16進を作る(t *testing.T) {
	id, err := domain.NewClusterID(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if err != nil || id != strings.Repeat("ab", 16) || !domain.ValidClusterID(id) {
		t.Errorf("%q %v", id, err)
	}
	if _, err := domain.NewClusterID(bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("乱数が足りないときはエラー")
	}
	if _, err := domain.NewClusterID(errReader{}); err == nil {
		t.Error("読めないときはエラー")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

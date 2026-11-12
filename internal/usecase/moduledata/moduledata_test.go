package moduledata_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/moduledata"
)

var ctx = context.Background()

func TestFor_モジュールごとに分かれた保存先になる(t *testing.T) {
	fs := &filestore.Store{Path: filepath.Join(t.TempDir(), "s.json")}
	auth, audit := moduledata.For(fs, "auth"), moduledata.For(fs, "audit")
	if err := auth.Put(ctx, "user/alice", []byte("a")); err != nil {
		t.Fatal(err)
	}
	_ = audit.Put(ctx, "user/alice", []byte("other"))
	if v, _ := auth.List(ctx); string(v["user/alice"]) != "a" || len(v) != 1 {
		t.Errorf("auth = %v", v)
	}
	_ = auth.Delete(ctx, "user/alice")
	if v, _ := auth.List(ctx); len(v) != 0 {
		t.Errorf("after delete = %v", v)
	}
	if v, _ := audit.List(ctx); string(v["user/alice"]) != "other" {
		t.Errorf("audit = %v", v)
	}
}

type mem map[string][]byte

func (m mem) ListModuleData(context.Context, string) (map[string][]byte, error) { return m, nil }
func (m mem) PutModuleData(_ context.Context, _, k string, v []byte) error {
	m[k] = v
	return nil
}
func (m mem) DeleteModuleData(_ context.Context, _, k string) error {
	delete(m, k)
	return nil
}

func TestPut_大きすぎる記録は断る(t *testing.T) {
	s := moduledata.For(mem{}, "auth")
	for name, put := range map[string]func() error{
		"empty key": func() error { return s.Put(ctx, "", nil) },
		"long key":  func() error { return s.Put(ctx, strings.Repeat("k", moduledata.MaxKey+1), nil) },
		"big value": func() error { return s.Put(ctx, "k", make([]byte, moduledata.MaxValue+1)) },
	} {
		if err := put(); !errors.Is(err, moduledata.ErrTooLarge) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The total: overwriting a key does not count it twice.
	big := make([]byte, moduledata.MaxValue)
	n := moduledata.MaxTotal / (moduledata.MaxValue + 8)
	for i := 0; i < n; i++ {
		if err := s.Put(ctx, "k"+strings.Repeat("0", 6-len(itoa(i)))+itoa(i), big); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if err := s.Put(ctx, "k000000", big); err != nil {
		t.Errorf("overwrite: %v", err)
	}
	if err := s.Put(ctx, "one-more", big); !errors.Is(err, moduledata.ErrTooLarge) {
		t.Errorf("over the total: %v", err)
	}
	many := moduledata.For(mem{}, "auth")
	for i := 0; i < moduledata.MaxRecords; i++ {
		_ = many.Put(ctx, itoa(i), nil)
	}
	if err := many.Put(ctx, "x", nil); !errors.Is(err, moduledata.ErrTooLarge) {
		t.Errorf("over the record count: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

type failing struct{}

func (failing) ListModuleData(context.Context, string) (map[string][]byte, error) {
	return nil, errors.New("disk")
}
func (failing) PutModuleData(context.Context, string, string, []byte) error { return nil }
func (failing) DeleteModuleData(context.Context, string, string) error      { return nil }

func Test初期化前や読めないときは失敗を返す(t *testing.T) {
	s := moduledata.For(nil, "auth")
	if _, err := s.List(ctx); !errors.Is(err, moduledata.ErrUnavailable) {
		t.Error("list")
	}
	if err := s.Put(ctx, "k", nil); !errors.Is(err, moduledata.ErrUnavailable) {
		t.Error("put")
	}
	if err := s.Delete(ctx, "k"); !errors.Is(err, moduledata.ErrUnavailable) {
		t.Error("delete")
	}
	if err := moduledata.For(failing{}, "auth").Put(ctx, "k", nil); err == nil {
		t.Error("list failure during put")
	}
}

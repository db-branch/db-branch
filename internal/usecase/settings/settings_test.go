package settings_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

func newService(t *testing.T) *settings.Service {
	return &settings.Service{
		Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")},
		Core: []ext.Setting{
			{Key: "branch.memory_mb", Type: "int", Default: "128"},
			{Key: "branch.default_ttl", Type: "duration", Default: ""},
			{Key: "name", Type: "string", Default: "x"},
		},
		Modules: func() map[string][]ext.Setting {
			return map[string][]ext.Setting{
				"proxysql": {{Key: "comment_routing", Type: "bool", Default: "true"}},
				"plain":    nil,
			}
		},
	}
}

func TestScopes_本体と設定を持つモジュール(t *testing.T) {
	sc := newService(t).Scopes()
	if len(sc) != 2 || sc[0].Name != "core" || sc[0].Title != "db-branch" || sc[1].Name != "proxysql" {
		t.Errorf("scopes = %+v", sc)
	}
	if len((&settings.Service{}).Scopes()) != 1 {
		t.Error("without modules: core only")
	}
}

func TestPut_型を確かめて保存し既定値で埋めて返す(t *testing.T) {
	s := newService(t)
	if v, _ := s.Values(ctx, "core"); v["branch.memory_mb"] != "128" || v["branch.default_ttl"] != "" {
		t.Errorf("defaults = %v", v)
	}
	if err := s.Put(ctx, "core", map[string]string{"branch.memory_mb": "256", "branch.default_ttl": "72h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "core", map[string]string{"name": "y"}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Values(ctx, "core")
	if v["branch.memory_mb"] != "256" || v["branch.default_ttl"] != "72h" || v["name"] != "y" {
		t.Errorf("values = %v", v)
	}
	_ = s.Put(ctx, "core", map[string]string{"branch.memory_mb": ""})
	if v, _ := s.Values(ctx, "core"); v["branch.memory_mb"] != "128" {
		t.Errorf("reset to default: %v", v)
	}
	if err := s.Put(ctx, "proxysql", map[string]string{"comment_routing": "false"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Values(ctx, "proxysql"); v["comment_routing"] != "false" {
		t.Errorf("module = %v", v)
	}
}

func TestPut_知らない項目や型違いや知らない範囲は断る(t *testing.T) {
	s := newService(t)
	for _, v := range []map[string]string{
		{"nope": "1"}, {"branch.memory_mb": "lots"}, {"branch.default_ttl": "soon"}, {"branch.default_ttl": "-1h"},
	} {
		if err := s.Put(ctx, "core", v); !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("%v: %v", v, err)
		}
	}
	if err := s.Put(ctx, "proxysql", map[string]string{"comment_routing": "maybe"}); !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("bool: %v", err)
	}
	if err := s.Put(ctx, "ghost", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown scope: %v", err)
	}
	if _, err := s.Values(ctx, "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown scope values: %v", err)
	}
}

type broken struct{}

func (broken) GetSettings(context.Context, string) (map[string]string, error) {
	return nil, errors.New("raft down")
}
func (broken) PutSettings(context.Context, string, map[string]string) error { return nil }

func Test読めなければエラー(t *testing.T) {
	s := newService(t)
	s.Store = broken{}
	if _, err := s.Values(ctx, "core"); err == nil {
		t.Error("values")
	}
	if err := s.Put(ctx, "core", nil); err == nil {
		t.Error("put")
	}
}

func TestPut_項目ごとの追加検査で断り通ったものは保存する(t *testing.T) {
	s := newService(t)
	s.Checks = map[string]func(string) error{
		"name": func(v string) error {
			if v == "bad" {
				return errors.New("name must not be bad")
			}
			return nil
		},
	}
	err := s.Put(ctx, "core", map[string]string{"name": "bad"})
	if !errors.Is(err, settings.ErrInvalid) || err.Error() == "" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "name must not be bad") {
		t.Errorf("the reason is lost: %v", err)
	}
	if err := s.Put(ctx, "core", map[string]string{"name": "good"}); err != nil {
		t.Fatal(err)
	}
	// An empty value restores the default and is never checked.
	if err := s.Put(ctx, "core", map[string]string{"name": ""}); err != nil {
		t.Fatal(err)
	}
}

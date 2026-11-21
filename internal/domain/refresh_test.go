package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func validRefresh() domain.BaseRefresh {
	n, _ := domain.ParseName("prod")
	return domain.BaseRefresh{
		Base:      n,
		Source:    domain.RefreshSource{Host: "db.example.com", Port: 5432, User: "dump_user", Password: "s3cret", Databases: []string{"app", "billing"}},
		Every:     24 * time.Hour,
		UpdatedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
	}
}

func TestBaseRefreshValidate_正しい設定を受け入れる(t *testing.T) {
	if err := validRefresh().Validate(domain.Postgres); err != nil {
		t.Fatal(err)
	}
}

func TestBaseRefreshValidate_不正な設定を理由つきで断る(t *testing.T) {
	cases := map[string]func(*domain.BaseRefresh){
		"interval":      func(r *domain.BaseRefresh) { r.Every = 59 * time.Minute },
		"host":          func(r *domain.BaseRefresh) { r.Source.Host = "" },
		"host-flag":     func(r *domain.BaseRefresh) { r.Source.Host = "-oProxy" },
		"host-space":    func(r *domain.BaseRefresh) { r.Source.Host = "a b" },
		"port-zero":     func(r *domain.BaseRefresh) { r.Source.Port = 0 },
		"port-big":      func(r *domain.BaseRefresh) { r.Source.Port = 70000 },
		"user":          func(r *domain.BaseRefresh) { r.Source.User = "" },
		"user-flag":     func(r *domain.BaseRefresh) { r.Source.User = "-x" },
		"password-nul":  func(r *domain.BaseRefresh) { r.Source.Password = "a\x00b" },
		"no-databases":  func(r *domain.BaseRefresh) { r.Source.Databases = nil },
		"database-flag": func(r *domain.BaseRefresh) { r.Source.Databases = []string{"--all"} },
		"database-dup":  func(r *domain.BaseRefresh) { r.Source.Databases = []string{"a", "a"} },
		"database-many": func(r *domain.BaseRefresh) {
			r.Source.Databases = nil
			for i := 0; i < domain.MaxRefreshDatabases+1; i++ {
				r.Source.Databases = append(r.Source.Databases, "d"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)))
			}
		},
	}
	for name, mutate := range cases {
		r := validRefresh()
		mutate(&r)
		if err := r.Validate(domain.Postgres); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBaseRefreshValidate_PostgreSQLとMySQL以外のエンジンは断る(t *testing.T) {
	if err := validRefresh().Validate(domain.Engine("sqlite")); err == nil {
		t.Error("unsupported engine accepted")
	}
}

func TestBaseRefreshNextRun_最後の実行か設定時刻から間隔後(t *testing.T) {
	r := validRefresh()
	if got, want := r.NextRun(), r.UpdatedAt.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("never run: %v want %v", got, want)
	}
	fin := r.UpdatedAt.Add(30 * time.Hour)
	r.LastRun = &domain.RefreshRun{StartedAt: fin.Add(-time.Minute), FinishedAt: fin, OK: false, Error: "boom"}
	if got, want := r.NextRun(), fin.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("after a failed run: %v want %v", got, want)
	}
	// Editing after the last run postpones the next one.
	r.UpdatedAt = fin.Add(time.Hour)
	if got, want := r.NextRun(), r.UpdatedAt.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("after an edit: %v want %v", got, want)
	}
}

func TestBaseRefreshDue_次回時刻を過ぎたら期限(t *testing.T) {
	r := validRefresh()
	if r.Due(r.NextRun().Add(-time.Second)) {
		t.Error("due too early")
	}
	if !r.Due(r.NextRun()) {
		t.Error("not due at the next run")
	}
}

func TestBaseRefreshValidate_エンジンの宣言したsource_dumpに従う(t *testing.T) {
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = domain.NewBuiltinRuntimes()
	defer func() { domain.DefaultRuntimes = old }()
	_ = domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "dumpable", Capabilities: domain.RuntimeCaps{SourceDump: true}})
	_ = domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "sealed"})

	if err := validRefresh().Validate("dumpable"); err != nil {
		t.Errorf("an engine that dumps sources = %v", err)
	}
	err := validRefresh().Validate("sealed")
	if !errors.Is(err, domain.ErrInvalidRefresh) || !strings.Contains(err.Error(), `engine "sealed" cannot be refreshed`) {
		t.Errorf("an engine without source_dump = %v", err)
	}
}

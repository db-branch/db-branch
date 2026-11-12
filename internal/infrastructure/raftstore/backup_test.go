package raftstore_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

var taken = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)

func TestBackup_設定と記録を書き出し別のクラスタに戻せる(t *testing.T) {
	src := newCluster(t, 1).leader(t)
	_ = raftstore.AdminStore{Store: src}.Save(auth.Credentials{Hash: []byte("pw-hash")})
	_ = src.PutSettings(ctx, "core", map[string]string{"license.key": "dbbl1.x", "branch.memory_mb": "512"})
	_ = src.PutToken(ctx, domain.APIToken{ID: "t1", Name: "ci", Scope: "full", Hash: "h1"})
	_ = src.PutModule(ctx, domain.ModuleSpec{Name: "auth", Version: "1.0", Wanted: true})
	_ = src.PutModuleData(ctx, "auth", "user/a@example.com", []byte(`{"email":"a@example.com"}`))
	_ = src.PutRouteUser(ctx, domain.RouteUser{Name: "app"})
	_ = src.AddRuntime(ctx, domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"})
	_ = src.PutBase(ctx, domain.Base{Name: name("prod")})
	_ = src.PutBranch(ctx, domain.Branch{Name: name("feature-x")})

	var buf bytes.Buffer
	if err := src.WriteBackup(&buf, "node1", taken); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	var plain bytes.Buffer
	_, _ = plain.ReadFrom(zr)
	if !strings.Contains(plain.String(), `"node":"node1"`) || strings.Contains(plain.String(), "join_secret") || strings.Contains(plain.String(), `"operations"`) {
		t.Errorf("backup content: %s", plain.String())
	}

	dst := newCluster(t, 1).leader(t)
	_ = dst.PutSettings(ctx, "core", map[string]string{"branch.default_ttl": "24h"})
	sum, err := dst.RestoreBackup(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	st := dst.State()
	if st.Admin == nil || string(st.Admin.Hash) != "pw-hash" {
		t.Errorf("admin %+v", st.Admin)
	}
	if st.Settings["core"]["license.key"] != "dbbl1.x" || st.Settings["core"]["branch.default_ttl"] != "24h" {
		t.Errorf("settings merged: %v", st.Settings)
	}
	if st.Tokens["t1"].Hash != "h1" || !st.Modules["auth"].Wanted || string(st.ModuleData["auth"]["user/a@example.com"]) == "" || len(st.RouteUsers) != 1 || !st.Runtimes["mysql@8.4.13"] {
		t.Errorf("state %+v", st)
	}
	// Bases and branches are recorded but not restored: their data is not in the backup.
	if len(st.Bases) != 0 || len(st.Branches) != 0 {
		t.Errorf("records restored without data: %v %v", st.Bases, st.Branches)
	}
	if sum.Tokens != 1 || sum.Settings != 1 || sum.ModuleData != 1 || strings.Join(sum.SkippedBases, ",") != "prod" || strings.Join(sum.SkippedBranches, ",") != "feature-x" || !sum.TakenAt.Equal(taken) || sum.Node != "node1" {
		t.Errorf("summary %+v", sum)
	}
}

func TestRestore_壊れたファイルや別形式は断る(t *testing.T) {
	dst := newCluster(t, 1).leader(t)
	if _, err := dst.RestoreBackup(ctx, strings.NewReader("not gzip")); err == nil {
		t.Error("not gzip")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"format":99}`))
	_ = zw.Close()
	if _, err := dst.RestoreBackup(ctx, bytes.NewReader(buf.Bytes())); !errors.Is(err, raftstore.ErrBackupFormat) {
		t.Errorf("unknown format: %v", err)
	}
	buf.Reset()
	zw = gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{`))
	_ = zw.Close()
	if _, err := dst.RestoreBackup(ctx, bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("truncated JSON")
	}
}

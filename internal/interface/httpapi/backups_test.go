package httpapi_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

type fakeBackups struct {
	files    []domain.BackupFile
	restored string
}

func (f *fakeBackups) Take(context.Context) (domain.BackupFile, error) {
	b := domain.BackupFile{Name: "db-branch-n1-20261110T030000Z.json.gz", Size: 10, TakenAt: t0}
	f.files = append([]domain.BackupFile{b}, f.files...)
	return b, nil
}
func (f *fakeBackups) List() ([]domain.BackupFile, error) { return f.files, nil }
func (f *fakeBackups) Open(name string) (io.ReadCloser, error) {
	for _, b := range f.files {
		if b.Name == name {
			return io.NopCloser(strings.NewReader("gzdata")), nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeBackups) Restore(_ context.Context, r io.Reader) (domain.RestoreSummary, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return domain.RestoreSummary{}, err
	}
	f.restored = string(b)
	return domain.RestoreSummary{Node: "n1", Tokens: 2, SkippedBases: []string{"prod"}, TakenAt: time.Unix(0, 0)}, nil
}

func TestBackups_一覧と今すぐ取得とダウンロード(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/backups", nil); resp.StatusCode != 501 {
		t.Errorf("without backups: %d", resp.StatusCode)
	}
	b := &fakeBackups{}
	f.srv.Backups = b
	resp, body := f.do(t, "POST", "/v1/backups", nil)
	if resp.StatusCode != 201 || body["name"] != "db-branch-n1-20261110T030000Z.json.gz" {
		t.Fatalf("take: %d %v", resp.StatusCode, body)
	}
	if _, body := f.do(t, "GET", "/v1/backups", nil); len(body["items"].([]any)) != 1 || body["node"] != "node1" {
		t.Errorf("list: %v", body)
	} else if v, ok := body["next_cursor"]; !ok || v != nil {
		t.Errorf("backups next_cursor = %v (present=%v), want null", v, ok)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/backups/db-branch-n1-20261110T030000Z.json.gz", nil))
	if rec.Code != 200 || rec.Body.String() != "gzdata" || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if resp, _ := f.do(t, "GET", "/v1/backups/nope.json.gz", nil); resp.StatusCode != 404 {
		t.Errorf("missing: %d", resp.StatusCode)
	}
}

func TestBackups_復元はノード上のソケットからだけ(t *testing.T) {
	f := newFixture(t)
	b := &fakeBackups{}
	f.srv.Backups = b
	req := httptest.NewRequest("POST", "/v1/backups/restore", strings.NewReader("gzipped state"))
	req.Header.Set("Content-Type", "application/gzip")
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, req)
	if rec.Code != 200 || b.restored != "gzipped state" || !strings.Contains(rec.Body.String(), `"skipped_bases":["prod"]`) {
		t.Errorf("socket: %d %s %q", rec.Code, rec.Body.String(), b.restored)
	}
	cookie, csrf := login(t, f)
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/backups/restore", map[string]any{}, withSession(cookie, csrf))
	if resp.StatusCode != 403 || errCode(body) != "socket_only" {
		t.Errorf("remote: %d %v", resp.StatusCode, body)
	}
}

func TestBackups_ファイル名のノードへ自動で回す(t *testing.T) {
	f, c := remoteFixture(t)
	f.srv.Backups = &fakeBackups{}
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/backups/db-branch-n2-20261110T030000Z.json.gz", nil))
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 GET /v1/backups/db-branch-n2-20261110T030000Z.json.gz") {
		t.Errorf("forwarded %v (%d)", c.forwarded, rec.Code)
	}
}

func TestBackups_復元の本文が上限を超えたら413を返す(t *testing.T) {
	f := newFixture(t)
	f.srv.Backups = &fakeBackups{}
	big := io.LimitReader(zeroReader{}, 65<<20)
	req := httptest.NewRequest("POST", "/v1/backups/restore", big)
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, req)
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "too_large") {
		t.Errorf("oversized body: %d %s", rec.Code, rec.Body.String())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

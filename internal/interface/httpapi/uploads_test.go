package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/uploads"
)

func uploadFixture(t *testing.T) (*fixture, *uploads.Store) {
	f := newFixture(t)
	store := &uploads.Store{
		Dir:      filepath.Join(t.TempDir(), "uploads"),
		MaxBytes: func() int64 { return 1000 },
		Free:     func(string) (uint64, error) { return 1 << 20, nil },
		Clock:    time.Now,
	}
	f.srv.Uploads = store
	f.srv.PoolFree = func() uint64 { return 1 << 20 }
	return f, store
}

func upload(t *testing.T, f *fixture, name, content string, mutate func(*http.Request)) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/uploads?name="+name, strings.NewReader(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Result(), out
}

func TestUpload_ダンプを送りベースを作れる(t *testing.T) {
	f, store := uploadFixture(t)
	resp, body := upload(t, f, "prod.sql.gz", "dump", nil)
	if resp.StatusCode != 201 || body["size"] != 4.0 || body["name"] != "prod.sql.gz" || body["node"] != "node1" ||
		!strings.HasPrefix(body["id"].(string), "upload:") {
		t.Fatalf("upload: %d %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	u, _ := store.Get(strings.TrimPrefix(id, "upload:"))
	resp, op := f.do(t, "POST", "/v1/bases", map[string]any{"name": "fresh", "runtime": "mysql@8.4.13", "dump": id})
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d %v", resp.StatusCode, op)
	}
	f.waitOp(t, op["id"].(string))
	if f.branches.calls[0] != "create-base fresh mysql@8.4.13 dump="+u.Path {
		t.Errorf("calls %v", f.branches.calls)
	}
	if _, err := os.Stat(u.Path); !os.IsNotExist(err) {
		t.Error("the upload should be removed once used")
	}
	// A used (or unknown) upload cannot be used again.
	if resp, _ := f.do(t, "POST", "/v1/bases", map[string]any{"name": "again", "runtime": "mysql@8.4.13", "dump": id}); resp.StatusCode != 404 {
		t.Errorf("reused: %d", resp.StatusCode)
	}
}

func TestLoad_ダンプをベースに流しスナップショットを取る(t *testing.T) {
	f, store := uploadFixture(t)
	_, body := upload(t, f, "more.sql", "INSERT", nil)
	id := body["id"].(string)
	u, _ := store.Get(strings.TrimPrefix(id, "upload:"))
	resp, op := f.do(t, "POST", "/v1/bases/prod/load", map[string]any{"dump": id, "snapshot": true})
	if resp.StatusCode != 202 || op["kind"] != "base.load" {
		t.Fatalf("load: %d %v", resp.StatusCode, op)
	}
	f.waitOp(t, op["id"].(string))
	if strings.Join(f.branches.calls, "|") != "load-base prod "+u.Path+"|snapshot prod" {
		t.Errorf("calls %v", f.branches.calls)
	}
	if _, err := os.Stat(u.Path); !os.IsNotExist(err) {
		t.Error("the upload should be removed once used")
	}
	f.branches.calls = nil
	resp, op = f.do(t, "POST", "/v1/bases/prod/load", map[string]any{"dump": "/srv/dumps/x.sql"})
	f.waitOp(t, op["id"].(string))
	if resp.StatusCode != 202 || strings.Join(f.branches.calls, "|") != "load-base prod /srv/dumps/x.sql" {
		t.Errorf("path dump: %d %v", resp.StatusCode, f.branches.calls)
	}
	f.branches.fail = domain.ErrConflict
	f.branches.calls = nil
	_, body = upload(t, f, "more.sql", "INSERT", nil)
	_, op = f.do(t, "POST", "/v1/bases/prod/load", map[string]any{"dump": body["id"], "snapshot": true})
	if got := f.waitOp(t, op["id"].(string)); got.Error == "" || strings.Contains(strings.Join(f.branches.calls, "|"), "snapshot") {
		t.Errorf("a failed load does not snapshot: %+v %v", got, f.branches.calls)
	}
	for _, c := range []struct {
		body map[string]any
		path string
		want int
	}{
		{map[string]any{}, "/v1/bases/prod/load", 400},
		{map[string]any{"dump": "upload:00000000000000000000000000000000"}, "/v1/bases/prod/load", 404},
		{map[string]any{"dump": "/x.sql"}, "/v1/bases/nope/load", 404},
	} {
		if resp, _ := f.do(t, "POST", c.path, c.body); resp.StatusCode != c.want {
			t.Errorf("%v: %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
}

func TestUpload_大きさと空き容量を確かめる(t *testing.T) {
	f, _ := uploadFixture(t)
	if resp, body := upload(t, f, "big.sql", strings.Repeat("x", 1001), nil); resp.StatusCode != 413 || errCode(body) != "too_large" {
		t.Errorf("too large: %d %v", resp.StatusCode, body)
	}
	f.srv.PoolFree = func() uint64 { return 10 }
	if resp, body := upload(t, f, "a.sql", strings.Repeat("x", 20), nil); resp.StatusCode != 507 || errCode(body) != "no_space" {
		t.Errorf("pool too small: %d %v", resp.StatusCode, body)
	}
	f.srv.PoolFree = func() uint64 { return 1 << 20 }
	if resp, _ := upload(t, f, "a.sql", "x", func(r *http.Request) { r.ContentLength = -1 }); resp.StatusCode != 411 {
		t.Errorf("unknown length: %d", resp.StatusCode)
	}
	if resp, _ := upload(t, f, "a.txt", "x", nil); resp.StatusCode != 400 {
		t.Errorf("bad name: %d", resp.StatusCode)
	}
	if resp, body := upload(t, f, "a.sql", "x", func(r *http.Request) { r.URL.RawQuery += "&node=elsewhere" }); resp.StatusCode != 201 {
		t.Errorf("single node ignores node: %d %v", resp.StatusCode, body)
	}
	f.srv.Uploads = nil
	if resp, _ := upload(t, f, "a.sql", "x", nil); resp.StatusCode != 501 {
		t.Errorf("no uploads: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/load", map[string]any{"dump": "upload:00000000000000000000000000000000"}); resp.StatusCode != 501 {
		t.Errorf("no uploads, load: %d", resp.StatusCode)
	}
}

func TestUpload_監査の名前読みで大きな本文が欠けない(t *testing.T) {
	f, store := uploadFixture(t)
	store.MaxBytes = func() int64 { return 8 << 20 }
	store.Free = func(string) (uint64, error) { return 64 << 20, nil }
	f.srv.PoolFree = func() uint64 { return 64 << 20 }
	big := strings.Repeat("INSERT INTO t VALUES (1);\n", 200000) // ~5 MiB
	resp, body := upload(t, f, "big.sql", big, func(r *http.Request) { r.Header.Del("Content-Type") })
	if resp.StatusCode != 201 || body["size"] != float64(len(big)) {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	u, _ := store.Get(strings.TrimPrefix(body["id"].(string), "upload:"))
	if b, _ := os.ReadFile(u.Path); len(b) != len(big) {
		t.Errorf("stored %d of %d bytes", len(b), len(big))
	}
}

func TestUploadRoom_送る前に送れる大きさが分かる(t *testing.T) {
	f, _ := uploadFixture(t) // limit 1000, disk 1 MiB, pool 1 MiB
	resp, body := f.do(t, "GET", "/v1/uploads/room", nil)
	if resp.StatusCode != 200 || body["max_bytes"] != 1000.0 || body["node"] != "node1" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
	f.srv.PoolFree = func() uint64 { return 300 }
	if _, body := f.do(t, "GET", "/v1/uploads/room", nil); body["max_bytes"] != 300.0 || body["reason"] != "pool" {
		t.Errorf("pool: %v", body)
	}
	f.srv.Uploads = nil
	if resp, _ := f.do(t, "GET", "/v1/uploads/room", nil); resp.StatusCode != 501 {
		t.Errorf("no uploads: %d", resp.StatusCode)
	}
}

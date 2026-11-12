package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
)

var ctx = context.Background()

const body = "runtime archive bytes"

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func server(t *testing.T, payload string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestGet_ダウンロードしてハッシュが一致すればパスを返す(t *testing.T) {
	srv, _ := server(t, body, http.StatusOK)
	c := artifact.Cache{Dir: t.TempDir(), Client: srv.Client()}

	path, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a.tar.xz", SHA256: digest(body), Size: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("content = %q", got)
	}
}

func TestGet_取得済みならダウンロードしない(t *testing.T) {
	srv, hits := server(t, body, http.StatusOK)
	c := artifact.Cache{Dir: t.TempDir(), Client: srv.Client()}
	a := domain.Artifact{URL: srv.URL + "/a.tar.xz", SHA256: digest(body)}

	first, _ := c.Get(ctx, a)
	second, err := c.Get(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || hits.Load() != 1 {
		t.Errorf("paths %q %q, hits %d; want same path and 1 hit", first, second, hits.Load())
	}
}

func TestGet_手元のファイルが壊れていれば取り直す(t *testing.T) {
	srv, hits := server(t, body, http.StatusOK)
	dir := t.TempDir()
	c := artifact.Cache{Dir: dir, Client: srv.Client()}
	a := domain.Artifact{URL: srv.URL + "/a.tar.xz", SHA256: digest(body)}
	path, _ := c.Get(ctx, a)
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Get(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body || hits.Load() != 2 {
		t.Errorf("content %q, hits %d", got, hits.Load())
	}
}

func TestGet_ハッシュが違えば破棄してエラーにする(t *testing.T) {
	srv, _ := server(t, "something else", http.StatusOK)
	dir := t.TempDir()
	c := artifact.Cache{Dir: dir, Client: srv.Client()}

	_, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body)})
	if !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestGet_サイズが宣言より大きければ途中で打ち切る(t *testing.T) {
	srv, _ := server(t, body, http.StatusOK)
	c := artifact.Cache{Dir: t.TempDir(), Client: srv.Client()}

	_, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body), Size: 3})
	if !errors.Is(err, artifact.ErrSizeMismatch) {
		t.Fatalf("error = %v, want ErrSizeMismatch", err)
	}
}

func TestGet_HTTPエラーは失敗にする(t *testing.T) {
	srv, _ := server(t, "nope", http.StatusNotFound)
	c := artifact.Cache{Dir: t.TempDir(), Client: srv.Client()}

	if _, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_HTTPSでないURLは拒否する(t *testing.T) {
	c := artifact.Cache{Dir: t.TempDir(), Client: http.DefaultClient}

	if _, err := c.Get(ctx, domain.Artifact{URL: "http://example.test/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_接続できなければ失敗にする(t *testing.T) {
	srv, _ := server(t, body, http.StatusOK)
	client := srv.Client()
	url := srv.URL
	srv.Close()
	c := artifact.Cache{Dir: t.TempDir(), Client: client}

	if _, err := c.Get(ctx, domain.Artifact{URL: url + "/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_保存先を作れなければ失敗にする(t *testing.T) {
	srv, _ := server(t, body, http.StatusOK)
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	c := artifact.Cache{Dir: filepath.Join(file, "sub"), Client: srv.Client()}

	if _, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_URLが壊れていれば失敗にする(t *testing.T) {
	c := artifact.Cache{Dir: t.TempDir(), Client: http.DefaultClient}

	if _, err := c.Get(ctx, domain.Artifact{URL: "https://bad host/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_通信が途中で切れたら失敗にする(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	c := artifact.Cache{Dir: dir, Client: srv.Client()}

	if _, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestGet_保存先に同名のディレクトリがあれば失敗にする(t *testing.T) {
	srv, _ := server(t, body, http.StatusOK)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, digest(body), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := artifact.Cache{Dir: dir, Client: srv.Client()}

	if _, err := c.Get(ctx, domain.Artifact{URL: srv.URL + "/a", SHA256: digest(body)}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestGet_取得元を差し替えても同じく検証する(t *testing.T) {
	data := []byte("from the leader")
	sum := sha256.Sum256(data)
	a := domain.Artifact{URL: "https://upstream.invalid/x", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	var asked []domain.Artifact
	c := artifact.Cache{Dir: t.TempDir(), Fetch: func(_ context.Context, got domain.Artifact) (io.ReadCloser, error) {
		asked = append(asked, got)
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
	path, err := c.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, data) || len(asked) != 1 {
		t.Errorf("file %q asked %v", b, asked)
	}
	bad := artifact.Cache{Dir: t.TempDir(), Fetch: func(context.Context, domain.Artifact) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("tampered bytes!")), nil
	}}
	if _, err := bad.Get(context.Background(), a); !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Errorf("tampered: %v", err)
	}
	failing := artifact.Cache{Dir: t.TempDir(), Fetch: func(context.Context, domain.Artifact) (io.ReadCloser, error) {
		return nil, errors.New("leader unreachable")
	}}
	if _, err := failing.Get(context.Background(), a); err == nil {
		t.Error("fetch failure: want error")
	}
}

func TestGet_取得元がURLを使うよう返せばURLから取る(t *testing.T) {
	data := []byte("upstream bytes")
	sum := sha256.Sum256(data)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	a := domain.Artifact{URL: srv.URL + "/x", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	c := artifact.Cache{Dir: t.TempDir(), Client: srv.Client(), Fetch: func(context.Context, domain.Artifact) (io.ReadCloser, error) {
		return nil, artifact.ErrUseURL
	}}
	if _, err := c.Get(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

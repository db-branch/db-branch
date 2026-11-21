package githubrelease_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/githubrelease"
)

func newTestTLSClient(srv *httptest.Server, allowedHosts []string) *http.Client {
	baseTr := srv.Client().Transport.(*http.Transport)
	tlsCfg := baseTr.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	tlsCfg.ServerName = "127.0.0.1"
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		Proxy: nil,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: artifact.PublicOnlyHostRedirect(allowedHosts),
	}
}

func fakePublicResolver(_ context.Context, _ string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

func TestGitHubRelease_タグ省略時は最新の非prereleaseを取得しdraftやprereleaseを拒否する(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotAccept, gotUA string
	var body string

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := &githubrelease.Client{
		HTTP:    newTestTLSClient(srv, githubrelease.AllowedAPIHosts()),
		Resolve: fakePublicResolver,
	}
	ref := domain.GitHubRef{Owner: "acme", Repo: "db-branch-rt-clickhouse"}

	// 正常な最新リリース
	body = `{
		"tag_name": "v0.2.0",
		"draft": false,
		"prerelease": false,
		"assets": [
			{
				"name": "db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz",
				"browser_download_url": "https://github.com/acme/db-branch-rt-clickhouse/releases/download/v0.2.0/db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz",
				"size": 12345
			}
		]
	}`
	rel, err := client.GetRelease(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetRelease() error = %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/repos/acme/db-branch-rt-clickhouse/releases/latest" {
		t.Errorf("path = %q, want /repos/acme/db-branch-rt-clickhouse/releases/latest", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (unauthenticated only)", gotAuth)
	}
	if gotAccept == "" || gotUA == "" {
		t.Errorf("Accept=%q User-Agent=%q, want both set", gotAccept, gotUA)
	}
	if rel.Tag != "v0.2.0" || rel.Draft || rel.Prerelease {
		t.Errorf("release = %+v", rel)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Name != "db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz" ||
		rel.Assets[0].DownloadURL != "https://github.com/acme/db-branch-rt-clickhouse/releases/download/v0.2.0/db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz" ||
		rel.Assets[0].Size != 12345 {
		t.Errorf("assets = %+v", rel.Assets)
	}

	// draft は拒否
	body = `{"tag_name": "v0.3.0", "draft": true, "prerelease": false, "assets": []}`
	if _, err := client.GetRelease(context.Background(), ref); !errors.Is(err, githubrelease.ErrDraftRelease) {
		t.Errorf("draft release err = %v, want ErrDraftRelease", err)
	}

	// prerelease は拒否
	body = `{"tag_name": "v0.3.0-rc.1", "draft": false, "prerelease": true, "assets": []}`
	if _, err := client.GetRelease(context.Background(), ref); !errors.Is(err, githubrelease.ErrPrerelease) {
		t.Errorf("prerelease err = %v, want ErrPrerelease", err)
	}
}

func TestGitHubRelease_タグ指定時は該当タグを取得し404や大きすぎるJSONを拒否する(t *testing.T) {
	var (
		gotURI     string
		statusCode int
		respHeader http.Header
		respBody   []byte
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		for k, vals := range respHeader {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write(respBody)
	}))
	defer srv.Close()

	client := &githubrelease.Client{
		HTTP:    newTestTLSClient(srv, githubrelease.AllowedAPIHosts()),
		Resolve: fakePublicResolver,
	}

	// 1. タグ指定取得成功
	statusCode = http.StatusOK
	respHeader = http.Header{"Content-Type": {"application/json"}}
	respBody = []byte(`{"tag_name":"v0.2.0","draft":false,"prerelease":false,"assets":[]}`)
	rel, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"})
	if err != nil {
		t.Fatalf("GetRelease(tag) error = %v", err)
	}
	if gotURI != "/repos/acme/rt-ch/releases/tags/v0.2.0" {
		t.Errorf("RequestURI = %q, want /repos/acme/rt-ch/releases/tags/v0.2.0", gotURI)
	}
	if rel.Tag != "v0.2.0" {
		t.Errorf("rel.Tag = %q, want v0.2.0", rel.Tag)
	}

	// 2. タグの url.PathEscape を確認する
	respBody = []byte(`{"tag_name":"v0.2.0/rc1","draft":false,"prerelease":false,"assets":[]}`)
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0/rc1"}); err != nil {
		t.Fatalf("GetRelease(escaped tag) error = %v", err)
	}
	if gotURI != "/repos/acme/rt-ch/releases/tags/v0.2.0%2Frc1" {
		t.Errorf("escaped RequestURI = %q, want /repos/acme/rt-ch/releases/tags/v0.2.0%%2Frc1", gotURI)
	}

	// 3. タグ指定でも prerelease は明示拒否
	respBody = []byte(`{"tag_name":"v0.2.0-rc.1","draft":false,"prerelease":true,"assets":[]}`)
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0-rc.1"}); !errors.Is(err, githubrelease.ErrPrerelease) {
		t.Errorf("tagged prerelease err = %v, want ErrPrerelease", err)
	}

	// 4. 404 は分かりやすいエラー（domain.ErrNotFound をラップ、タグ指定・省略とも）
	statusCode = http.StatusNotFound
	respHeader = nil
	respBody = []byte(`{"message":"Not Found"}`)
	_, err = client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v9.9.9"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("tagged 404 err = %v, want domain.ErrNotFound", err)
	}
	_, err = client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("latest 404 err = %v, want domain.ErrNotFound", err)
	}

	// 5. レート制限（403 / 429 かつ X-RateLimit-Remaining: 0）は専用メッセージ（Reset 有無とも）
	for _, tc := range []struct {
		code  int
		reset string
	}{
		{http.StatusForbidden, "1700000000"},
		{http.StatusTooManyRequests, ""},
	} {
		statusCode = tc.code
		respHeader = http.Header{"X-RateLimit-Remaining": {"0"}}
		if tc.reset != "" {
			respHeader.Set("X-RateLimit-Reset", tc.reset)
		}
		respBody = []byte(`{"message":"API rate limit exceeded"}`)
		_, err = client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"})
		if !errors.Is(err, githubrelease.ErrRateLimited) {
			t.Errorf("rate limit status %d err = %v, want ErrRateLimited", tc.code, err)
		}
	}

	// 5b. レート制限以外の 403 や 500 は ErrRateLimited にせず HTTP エラーを返す
	statusCode = http.StatusForbidden
	respHeader = http.Header{"X-RateLimit-Remaining": {"59"}}
	respBody = []byte(`{"message":"forbidden"}`)
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"}); err == nil || errors.Is(err, githubrelease.ErrRateLimited) {
		t.Errorf("non-ratelimit 403 err = %v, want generic API error", err)
	}

	// 6. 2 MiB 超 JSON 応答を拒否（Content-Length 事前拒否と chunked ストリーム打ち切りの両方）
	oversized := make([]byte, githubrelease.MaxReleaseJSONBytes+64)
	for i := range oversized {
		oversized[i] = ' '
	}
	copy(oversized, `{"tag_name":"v0.2.0","body":"`)
	copy(oversized[len(oversized)-3:], `"}`)
	statusCode = http.StatusOK
	respHeader = http.Header{"Content-Length": {strconv.Itoa(len(oversized))}}
	respBody = oversized
	_, err = client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"})
	if !errors.Is(err, githubrelease.ErrResponseTooLarge) {
		t.Errorf("oversized JSON (Content-Length) err = %v, want ErrResponseTooLarge", err)
	}

	// chunked（Content-Length なし）での 2 MiB 超応答も拒否する
	chunkSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte(" "), 64*1024)
		for written := int64(0); written <= githubrelease.MaxReleaseJSONBytes; written += int64(len(chunk)) {
			if _, wErr := w.Write(chunk); wErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer chunkSrv.Close()
	chunkClient := &githubrelease.Client{
		APIBase: "https://api.github.com/",
		HTTP:    newTestTLSClient(chunkSrv, githubrelease.AllowedAPIHosts()),
		Resolve: fakePublicResolver,
	}
	if _, err := chunkClient.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"}); !errors.Is(err, githubrelease.ErrResponseTooLarge) {
		t.Errorf("oversized chunked JSON err = %v, want ErrResponseTooLarge", err)
	}

	// 7. 不正 JSON と tag_name 欠落を拒否
	statusCode = http.StatusOK
	respHeader = nil
	respBody = []byte(`{broken`)
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"}); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("broken JSON err = %v, want decode error", err)
	}
	respBody = []byte(`{"draft":false,"prerelease":false}`)
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"}); err == nil || !strings.Contains(err.Error(), "missing tag_name") {
		t.Errorf("missing tag_name err = %v, want missing tag_name error", err)
	}

	// 8. 応答ボディ読取エラーを返す
	readErr := errors.New("connection reset")
	errReadClient := &githubrelease.Client{
		HTTP: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       errReadCloser{err: readErr},
				}, nil
			}),
		},
		Resolve: fakePublicResolver,
	}
	if _, err := errReadClient.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v0.2.0"}); !errors.Is(err, readErr) {
		t.Errorf("body read err = %v, want %v", err, readErr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

func TestGitHubRelease_Assetダウンロードでサイズ上限超過と非200応答を拒否する(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://objects.githubusercontent.com/ok", http.StatusFound)
		case "/ok":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("0123456789"))
		case "/declared-too-large":
			w.Header().Set("Content-Length", strconv.Itoa(100))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("a"), 100))
		case "/stream-too-large":
			// Content-Length を送らずに chunked で上限超えを送る
			flusher, _ := w.(http.Flusher)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("012345"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("67890"))
		case "/status-500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/status-404":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := &githubrelease.Client{
		AssetHTTP: newTestTLSClient(srv, githubrelease.AllowedAssetHosts()),
		Resolve:   fakePublicResolver,
	}

	// 1. 許可ホスト間のリダイレクトを経て maxBytes 以内を正常ダウンロード
	var buf bytes.Buffer
	n, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://github.com/redirect",
	}, 10, &buf)
	if err != nil {
		t.Fatalf("DownloadAsset(ok) error = %v", err)
	}
	if n != 10 || buf.String() != "0123456789" {
		t.Errorf("got (%d, %q), want (10, 0123456789)", n, buf.String())
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty", gotAuth)
	}

	// 2. Content-Length が maxBytes 超なら読む前に拒否（dst に何も書かれない）
	buf.Reset()
	n, err = client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://release-assets.githubusercontent.com/declared-too-large",
	}, 10, &buf)
	if !errors.Is(err, githubrelease.ErrResponseTooLarge) {
		t.Errorf("declared too large err = %v, want ErrResponseTooLarge", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("expected no bytes written before rejecting Content-Length, got n=%d buf=%d", n, buf.Len())
	}

	// 3. ストリーム実測が maxBytes+1 を超えたら打ち切りエラー
	buf.Reset()
	_, err = client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://github-releases.githubusercontent.com/stream-too-large",
	}, 10, &buf)
	if !errors.Is(err, githubrelease.ErrResponseTooLarge) {
		t.Errorf("stream too large err = %v, want ErrResponseTooLarge", err)
	}

	// 4. 非 200 応答はエラー
	for _, path := range []string{"/status-500", "/status-404"} {
		buf.Reset()
		if _, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
			Name:        "mod.tar.gz",
			DownloadURL: "https://objects.githubusercontent.com" + path,
		}, 100, &buf); err == nil {
			t.Errorf("%s was accepted, want error", path)
		}
	}

	// 5. maxBytes <= 0 や dst == nil を拒否
	if _, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://objects.githubusercontent.com/ok",
	}, 0, &buf); err == nil {
		t.Error("maxBytes=0 was accepted")
	}
	if _, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://objects.githubusercontent.com/ok",
	}, 10, nil); err == nil {
		t.Error("dst=nil was accepted")
	}

	// 6. dst の書き込みエラーを返す
	errWrite := errors.New("disk full")
	if _, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://objects.githubusercontent.com/ok",
	}, 100, failingWriter{err: errWrite}); !errors.Is(err, errWrite) {
		t.Errorf("failing writer err = %v, want %v", err, errWrite)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestGitHubRelease_SSRFと未許可ホストへのリダイレクトと非公開IPを拒否する(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-evil":
			http.Redirect(w, r, "https://evil.example.com/asset.tar.gz", http.StatusFound)
		case "/redirect-private-ip":
			http.Redirect(w, r, "https://127.0.0.1/asset.tar.gz", http.StatusFound)
		case "/redirect-http":
			http.Redirect(w, r, "http://objects.githubusercontent.com/asset.tar.gz", http.StatusFound)
		case "/repos/acme/rt-ch/releases/latest":
			// API 照会クライアントは api.github.com 以外（objects.githubusercontent.com 含む）へのリダイレクトを拒否する
			http.Redirect(w, r, "https://objects.githubusercontent.com/release.json", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := &githubrelease.Client{
		HTTP:      newTestTLSClient(srv, githubrelease.AllowedAPIHosts()),
		AssetHTTP: newTestTLSClient(srv, githubrelease.AllowedAssetHosts()),
		Resolve:   fakePublicResolver,
	}

	// 1. Asset ダウンロードのリダイレクト先検査（未許可ホスト / 非公開 IP / http）
	for name, path := range map[string]string{
		"未許可ホストへのリダイレクト": "/redirect-evil",
		"非公開IPへのリダイレクト":  "/redirect-private-ip",
		"httpへのリダイレクト":   "/redirect-http",
	} {
		var buf bytes.Buffer
		_, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
			Name:        "mod.tar.gz",
			DownloadURL: "https://github.com" + path,
		}, 1024, &buf)
		if !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s: err = %v, want ErrUnsafeURL", name, err)
		}
	}

	// 2. API クライアントは api.github.com のみ許可（asset 許可ホストへのリダイレクトも拒否）
	_, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch"})
	if !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("API redirect to objects.githubusercontent.com: err = %v, want ErrUnsafeURL", err)
	}

	// 3. 初期 URL のホスト許可・スキーム・ポート・認証情報チェック
	for name, rawURL := range map[string]string{
		"空URL":         "",
		"不正URL":        "://bad",
		"ホスト欠落":        "https:///a.tar.gz",
		"httpスキーム":     "http://github.com/a.tar.gz",
		"認証情報付き":       "https://user:pass@github.com/a.tar.gz",
		"非443ポート":      "https://github.com:8443/a.tar.gz",
		"未許可ホスト":       "https://evil.example.com/a.tar.gz",
		"サブドメイン偽装":     "https://sub.github.com/a.tar.gz",
		"プライベートIPリテラル": "https://127.0.0.1/a.tar.gz",
	} {
		var buf bytes.Buffer
		_, err := client.DownloadAsset(context.Background(), githubrelease.Asset{
			Name:        "mod.tar.gz",
			DownloadURL: rawURL,
		}, 1024, &buf)
		if !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("initial URL %s (%q): err = %v, want ErrUnsafeURL", name, rawURL, err)
		}
	}

	// 4. DNS 解決結果が非公開 IP なら ValidateCatalogURL で拒否
	privateClient := &githubrelease.Client{
		HTTP:      newTestTLSClient(srv, githubrelease.AllowedAPIHosts()),
		AssetHTTP: newTestTLSClient(srv, githubrelease.AllowedAssetHosts()),
		Resolve: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		},
	}
	if _, err := privateClient.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch"}); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("GetRelease private DNS err = %v, want ErrUnsafeURL", err)
	}
	var buf bytes.Buffer
	if _, err := privateClient.DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://objects.githubusercontent.com/ok",
	}, 1024, &buf); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("DownloadAsset private DNS err = %v, want ErrUnsafeURL", err)
	}

	// 5. ゼロ値 Client（既定の 2 つの公開専用クライアント & SystemResolver）が安全に拒否する
	zeroClient := &githubrelease.Client{APIBase: "https://127.0.0.1"}
	if _, err := zeroClient.GetRelease(context.Background(), domain.GitHubRef{Owner: "acme", Repo: "rt-ch"}); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("zeroClient GetRelease err = %v, want ErrUnsafeURL", err)
	}
	if _, err := (&githubrelease.Client{}).DownloadAsset(context.Background(), githubrelease.Asset{
		Name:        "mod.tar.gz",
		DownloadURL: "https://127.0.0.1/a.tar.gz",
	}, 1024, &buf); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("zeroClient DownloadAsset err = %v, want ErrUnsafeURL", err)
	}

	// 6. API 照会の不正な ref を拒否
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "", Repo: "rt-ch"}); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("empty owner err = %v, want ErrInvalidRef", err)
	}
	if _, err := client.GetRelease(context.Background(), domain.GitHubRef{Owner: "../evil", Repo: "rt-ch"}); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("traversal owner err = %v, want ErrInvalidRef", err)
	}

	// 7. 既定の許可ホスト一覧の内容を検証（API と Asset が分離されている）
	apiHosts := githubrelease.AllowedAPIHosts()
	assetHosts := githubrelease.AllowedAssetHosts()
	if len(apiHosts) != 1 || apiHosts[0] != "api.github.com" {
		t.Errorf("AllowedAPIHosts = %v, want [api.github.com]", apiHosts)
	}
	wantAssetHosts := "github.com,api.github.com,objects.githubusercontent.com,release-assets.githubusercontent.com,github-releases.githubusercontent.com"
	if strings.Join(assetHosts, ",") != wantAssetHosts {
		t.Errorf("AllowedAssetHosts = %v, want %s", assetHosts, wantAssetHosts)
	}
}

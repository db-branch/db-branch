package licenseclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/licenseclient"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
)

var ctx = context.Background()

func TestRenew_新しい鍵を受け取り送るのは鍵とUser_Agentだけ(t *testing.T) {
	var got struct {
		method, ua, ct, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.ua, got.ct, got.body = r.Method, r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), string(b)
		w.Write([]byte(`{"key":"dbbl1.new"}`))
	}))
	defer srv.Close()
	key, err := (&licenseclient.Client{Version: "1.4.2"}).Renew(ctx, srv.URL+"/renew", "dbbl1.old")
	if err != nil || key != "dbbl1.new" {
		t.Fatalf("%q %v", key, err)
	}
	var in map[string]string
	if json.Unmarshal([]byte(got.body), &in) != nil || len(in) != 1 || in["key"] != "dbbl1.old" {
		t.Errorf("本文: %s", got.body)
	}
	if got.method != "POST" || got.ua != "db-branch/1.4.2" || got.ct != "application/json" {
		t.Errorf("%+v", got)
	}
}

func TestRenew_応答ごとに意味のあるエラーを返す(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		is     error
		text   string
	}{
		{"204 まだ新しい鍵がない", 204, "", licensing.ErrNoNewerKey, ""},
		{"402 解約や未払い", 402, `{"error":{"code":"subscription_inactive","message":"card declined"}}`, licensing.ErrSubscriptionInactive, "card declined"},
		{"400 鍵を知らない", 400, `{"error":{"code":"bad_key","message":"unknown key"}}`, licensing.ErrKeyNotRecognised, "unknown key"},
		{"401 鍵を知らない", 401, `{"error":{"code":"unauthorized","message":"no"}}`, licensing.ErrKeyNotRecognised, "no"},
		{"402 本文が壊れている", 402, `not json`, licensing.ErrSubscriptionInactive, ""},
		{"500", 500, `oops`, nil, "500"},
		{"200 だが鍵がない", 200, `{"key":" "}`, nil, "without a key"},
		{"200 だが JSON でない", 200, `<html>`, nil, "without a key"},
		{"リダイレクトは辿らない", 302, ``, nil, "302"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.status == 302 {
				w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			}
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		_, err := (&licenseclient.Client{}).Renew(ctx, srv.URL, "k")
		srv.Close()
		if err == nil || c.is != nil && !errors.Is(err, c.is) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestRenew_応答がないときは時間切れで失敗する(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := &licenseclient.Client{HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	start := time.Now()
	if _, err := c.Renew(ctx, srv.URL, "k"); err == nil || !strings.Contains(err.Error(), "contacting") {
		t.Errorf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("遅すぎる")
	}
	if licenseclient.Timeout != 15*time.Second {
		t.Error("契約は15秒")
	}
}

func TestRenew_httpの更新URLは通信せずに断る(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	other := strings.Replace(srv.URL, "127.0.0.1", "licenses.example.com", 1)
	if _, err := (&licenseclient.Client{}).Renew(ctx, other, "k"); !errors.Is(err, licensing.ErrInsecureURL) || called {
		t.Errorf("%v %v", err, called)
	}
}

func TestRenew_つながらないときや不正な要求はエラーにする(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := (&licenseclient.Client{}).Renew(ctx, url, "k"); err == nil || !strings.Contains(err.Error(), "contacting") {
		t.Errorf("%v", err)
	}
	if _, err := (&licenseclient.Client{}).Renew(ctx, "http://127.0.0.1:1/\x7f", "k"); err == nil {
		t.Error("不正な URL")
	}
	// 本文の途中で切れる。
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.Write([]byte("{"))
	}))
	defer cut.Close()
	if _, err := (&licenseclient.Client{}).Renew(ctx, cut.URL, "k"); err == nil {
		t.Error("途中で切れた応答")
	}
}

func TestRenew_429はRetry_Afterの秒数を持つ待ちのエラーになる(t *testing.T) {
	for header, want := range map[string]time.Duration{"120": 2 * time.Minute, "": time.Hour, "abc": time.Hour, "-5": time.Hour, "99999999": 7 * 24 * time.Hour} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
		}))
		_, err := (&licenseclient.Client{}).Renew(ctx, srv.URL, "k")
		srv.Close()
		var later *licensing.RetryLaterError
		if !errors.As(err, &later) || later.After != want {
			t.Errorf("%q: %v", header, err)
		}
	}
}

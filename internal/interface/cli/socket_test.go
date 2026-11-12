package cli_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

func serveSocket(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func TestSocketClient_JSONを送り受け取る(t *testing.T) {
	path := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.URL.Path != "/v1/branches" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	var out struct{ OK bool }
	if err := cli.NewSocketClient(path).Do(context.Background(), "POST", "/v1/branches", map[string]int{"a": 1}, &out); err != nil || !out.OK {
		t.Errorf("Do = %v, %+v", err, out)
	}
}

func TestSocketClient_本文なしの応答とエラーの応答(t *testing.T) {
	path := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"branch x is resetting"}}`))
	})
	c := cli.NewSocketClient(path)
	var out map[string]any
	if err := c.Do(context.Background(), "DELETE", "/v1/x", nil, &out); err != nil {
		t.Errorf("204: %v", err)
	}
	err := c.Do(context.Background(), "POST", "/v1/x", nil, nil)
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "conflict" || !strings.Contains(err.Error(), "resetting") {
		t.Errorf("error = %v", err)
	}
}

func TestSocketClient_エージェントに繋がらなければ案内する(t *testing.T) {
	c := cli.NewSocketClient(filepath.Join(t.TempDir(), "missing.sock"))
	err := c.Do(context.Background(), "GET", "/v1/branches", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "systemctl status db-branch") {
		t.Errorf("error = %v", err)
	}
	if err := c.Do(context.Background(), "GET", "/v1/x", make(chan int), nil); err == nil {
		t.Error("unencodable body: want error")
	}
	if err := c.Do(context.Background(), "BAD METHOD", "/v1/x", nil, nil); err == nil {
		t.Error("bad method: want error")
	}
}

func Test待っている間に中断されたら止める(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/reset"] = accepted("op_1", "branch.reset")
	api.opStates = []map[string]any{{"id": "op_1", "state": "running", "phase": "rolling_back"}}
	var out, errOut strings.Builder
	app := cli.App{Stdout: &out, Stderr: &errOut, Client: func(string) cli.Client { return api }, Poll: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	code := app.Run(ctx, []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "branch", "reset", "x"})
	if code == 0 || !strings.Contains(errOut.String(), "deadline") {
		t.Errorf("exit %d stderr %q", code, errOut.String())
	}
}

func Testオペレーションの確認に失敗したらエラー(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/bases/prod/snapshots"] = accepted("op_1", "base.snapshot")
	if code, _, errOut := runAPI(t, api, "base", "snapshot", "prod"); code == 0 || !strings.Contains(errOut, "not stubbed") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func Test詳細表示はJSONでも出せて失敗理由と容量を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "failed", "status_reason": "clone failed",
		"storage": map[string]any{"used": 2048, "referenced": 1 << 30}}
	api.answers["GET /v1/bases/prod"] = map[string]any{"name": "prod", "status": "failed", "status_reason": "load failed",
		"snapshots": []any{map[string]any{"name": "s1", "created_at": "2026-11-09T12:00:00Z"}}}
	api.answers["GET /v1/bases"] = map[string]any{"items": []any{map[string]any{"name": "prod"}}}

	_, out, _ := runAPI(t, api, "branch", "show", "x")
	for _, want := range []string{"clone failed", "2.0 KiB own", "1.0 GiB total"} {
		if !strings.Contains(out, want) {
			t.Errorf("branch show lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runAPI(t, api, "base", "show", "prod")
	if !strings.Contains(out, "load failed") || !strings.Contains(out, "s1") {
		t.Errorf("base show:\n%s", out)
	}
	for _, args := range [][]string{{"branch", "show", "x", "-o", "json"}, {"base", "show", "prod", "-o", "json"}, {"base", "list", "-o", "json"}} {
		if _, out, _ := runAPI(t, api, args...); !strings.HasPrefix(strings.TrimSpace(out), "{") && !strings.HasPrefix(strings.TrimSpace(out), "[") {
			t.Errorf("%v: %q", args, out)
		}
	}
}

func Test一覧や表示や細かな操作のAPIエラーはそのまま返す(t *testing.T) {
	api := newFakeAPI()
	for _, args := range [][]string{
		{"branch", "list"}, {"branch", "dsn", "x"}, {"branch", "extend", "x", "1h"}, {"base", "list"}, {"base", "show", "x"},
		{"runtime", "list"}, {"runtime", "remove", "mysql@8.4.13"}, {"runtime", "install", "mysql@8.4.13"},
		{"branch", "create", "x", "--base", "Bad"},
	} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

func Test初期化とパスワード再設定の失敗はエラー(t *testing.T) {
	boom := errors.New("boom")
	app := cli.App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{},
		Init:          func(context.Context, config.Config, string) (string, error) { return "", boom },
		ResetPassword: func(config.Config) (string, error) { return "", boom },
		Client:        func(string) cli.Client { return cli.NewSocketClient(filepath.Join(t.TempDir(), "none.sock")) }}
	for _, args := range [][]string{{"init", "--pool", "tank"}, {"admin", "reset-password"}} {
		if code := app.Run(context.Background(), append([]string{"--config", filepath.Join(t.TempDir(), "x.toml")}, args...)); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

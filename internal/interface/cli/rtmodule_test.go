package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/interface/cli"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

const (
	testFP1 = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	testFP2 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type fakeRTModuleService struct {
	prompter rtmodule.TrustPrompter

	installReq rtmodule.InstallRequest
	installFn  func(ctx context.Context, req rtmodule.InstallRequest, prompter rtmodule.TrustPrompter) (rtmodule.InstallResult, error)
	listRes    []rtmodule.ModuleInfo
	removeArgs struct {
		called    bool
		engine    string
		noRestart bool
	}
	removeFn    func(ctx context.Context, engine string, noRestart bool) (rtmodule.RemoveResult, error)
	trustAddFn  func(keyID string, pubRaw []byte, expected domain.Fingerprint) (rtmodule.TrustKey, error)
	trustListFn func() ([]rtmodule.TrustKey, error)
	trustRmFn   func(ctx context.Context, keyID string) error
}

func (f *fakeRTModuleService) Install(ctx context.Context, req rtmodule.InstallRequest) (rtmodule.InstallResult, error) {
	f.installReq = req
	if f.installFn != nil {
		return f.installFn(ctx, req, f.prompter)
	}
	return rtmodule.InstallResult{}, errors.New("install not stubbed")
}

func (f *fakeRTModuleService) List() []rtmodule.ModuleInfo {
	return f.listRes
}

func (f *fakeRTModuleService) Remove(ctx context.Context, engine string, noRestart bool) (rtmodule.RemoveResult, error) {
	f.removeArgs.called = true
	f.removeArgs.engine = engine
	f.removeArgs.noRestart = noRestart
	if f.removeFn != nil {
		return f.removeFn(ctx, engine, noRestart)
	}
	return rtmodule.RemoveResult{}, errors.New("remove not stubbed")
}

func (f *fakeRTModuleService) TrustAdd(keyID string, pubRaw []byte, expected domain.Fingerprint) (rtmodule.TrustKey, error) {
	if f.trustAddFn != nil {
		return f.trustAddFn(keyID, pubRaw, expected)
	}
	return rtmodule.TrustKey{}, errors.New("trust add not stubbed")
}

func (f *fakeRTModuleService) TrustList() ([]rtmodule.TrustKey, error) {
	if f.trustListFn != nil {
		return f.trustListFn()
	}
	return nil, errors.New("trust list not stubbed")
}

func (f *fakeRTModuleService) TrustRemove(ctx context.Context, keyID string) error {
	if f.trustRmFn != nil {
		return f.trustRmFn(ctx, keyID)
	}
	return errors.New("trust remove not stubbed")
}

type rtTestEnv struct {
	api   *fakeAPI
	svc   *fakeRTModuleService
	uid   int
	tty   bool
	stdin string
}

func (e *rtTestEnv) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := cli.App{
		Version: "test",
		Stdin:   strings.NewReader(e.stdin),
		Stdout:  &out,
		Stderr:  &errOut,
		Client:  func(string) cli.Client { return e.api },
		RuntimeModules: func(_ config.Config, p rtmodule.TrustPrompter) cli.RuntimeModuleService {
			e.svc.prompter = p
			return e.svc
		},
		EffectiveUID: func() int { return e.uid },
		IsTTY:        func(io.Reader) bool { return e.tty },
	}
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	code := app.Run(context.Background(), append([]string{"--config", cfgPath}, args...))
	return code, out.String(), errOut.String()
}

func TestCLI_module_installがgithub参照をローカル導入へ単一モジュール名をAPIへ振り分ける(t *testing.T) {
	fp1, err := domain.ParseFingerprint(testFP1)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("単一モジュール名は従来のAPT APIへ送られる", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 1000}
		env.api.answers["POST /v1/modules"] = map[string]any{"name": "proxysql", "version": "1.0", "wanted": true}

		code, out, errOut := env.run(t, "module", "install", "proxysql", "--version", "1.0")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !strings.Contains(out, "proxysql 1.0") {
			t.Errorf("stdout = %q", out)
		}
		if env.api.bodies["POST /v1/modules"].(map[string]any)["name"] != "proxysql" {
			t.Errorf("unexpected API call: %v", env.api.bodies)
		}
	})

	t.Run("github参照はローカル導入されTTY対話yで鍵信頼とクラスタ案内を出力する", func(t *testing.T) {
		env := &rtTestEnv{
			api:   newFakeAPI(),
			svc:   &fakeRTModuleService{},
			uid:   0,
			tty:   true,
			stdin: "y\n",
		}
		env.svc.installFn = func(ctx context.Context, req rtmodule.InstallRequest, prompter rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			ok, err := prompter.ConfirmTrust(ctx, "acme-2026", fp1, req.Ref, "v0.2.0")
			if err != nil {
				return rtmodule.InstallResult{}, err
			}
			if !ok {
				return rtmodule.InstallResult{}, fmt.Errorf("%w: declined", domain.ErrKeyUntrusted)
			}
			return rtmodule.InstallResult{
				Engine:         "clickhouse",
				Version:        "0.2.0",
				Tag:            "v0.2.0",
				KeyID:          "acme-2026",
				KeyFingerprint: fp1,
				User:           "db-branch-rt-clickhouse",
				UserCreated:    true,
				KeyAdded:       true,
				Installed:      true,
				Restarted:      true,
			}, nil
		}

		code, out, errOut := env.run(t, "module", "install", "github.com/acme/db-branch-rt-clickhouse@v0.2.0")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if len(env.api.bodies) != 0 {
			t.Errorf("should not call API, got %v", env.api.bodies)
		}
		if env.svc.installReq.Ref.String() != "github.com/acme/db-branch-rt-clickhouse@v0.2.0" {
			t.Errorf("ref = %q", env.svc.installReq.Ref.String())
		}
		for _, want := range []string{
			"Untrusted signing key:",
			"acme-2026",
			testFP1,
			"out-of-band",
			"Trust this key and save to /etc/db-branch/trusted-keys/acme-2026.pub? [y/N]:",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q:\n%s", want, errOut)
			}
		}
		for _, want := range []string{
			"Fetching release v0.2.0 from github.com/acme/db-branch-rt-clickhouse",
			"Trusted key saved: /etc/db-branch/trusted-keys/acme-2026.pub",
			"Created system user: db-branch-rt-clickhouse",
			"Installed runtime module clickhouse 0.2.0 to /usr/lib/db-branch/runtimes/clickhouse",
			"Restarted db-branch.service.",
			"github.com/acme/db-branch-rt-clickhouse@v0.2.0 --trust-key " + testFP1,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("TTY対話でNを選ぶと終了コード3で拒否する", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0, tty: true, stdin: "n\n"}
		env.svc.installFn = func(ctx context.Context, req rtmodule.InstallRequest, prompter rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			ok, err := prompter.ConfirmTrust(ctx, "acme-2026", fp1, req.Ref, "v0.2.0")
			if err != nil {
				return rtmodule.InstallResult{}, err
			}
			if !ok {
				return rtmodule.InstallResult{}, fmt.Errorf("%w: not approved", domain.ErrKeyUntrusted)
			}
			return rtmodule.InstallResult{}, nil
		}

		code, _, errOut := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.2.0")
		if code != 3 {
			t.Fatalf("code = %d, want 3; stderr = %q", code, errOut)
		}
	})

	t.Run("非TTYでは対話せずtrust-keyの案内を出して終了コード3になる", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0, tty: false, stdin: "y\n"}
		env.svc.installFn = func(ctx context.Context, req rtmodule.InstallRequest, prompter rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			_, err := prompter.ConfirmTrust(ctx, "acme-2026", fp1, req.Ref, "v0.2.0")
			return rtmodule.InstallResult{}, err
		}

		code, _, errOut := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.2.0")
		if code != 3 {
			t.Fatalf("code = %d, want 3; stderr = %q", code, errOut)
		}
		if !strings.Contains(errOut, "--trust-key "+testFP1) || !strings.Contains(errOut, "out-of-band") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("trust-keyとupgradeとno-restartを渡しJSON出力できる", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.installFn = func(_ context.Context, req rtmodule.InstallRequest, _ rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			return rtmodule.InstallResult{
				Engine:          "clickhouse",
				Version:         "0.3.0",
				PreviousVersion: "0.2.0",
				Tag:             "v0.3.0",
				KeyID:           "acme-2026",
				KeyFingerprint:  req.TrustKey,
				Upgraded:        true,
				User:            "db-branch-rt-clickhouse",
				UserUID:         984,
				UserGID:         984,
				Installed:       true,
			}, nil
		}

		code, out, errOut := env.run(t, "-o", "json", "module", "install",
			"github.com/acme/rt-ch", "--trust-key", testFP1, "--upgrade", "--no-restart")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !env.svc.installReq.Upgrade || !env.svc.installReq.NoRestart || env.svc.installReq.TrustKey.String() != testFP1 {
			t.Errorf("req = %+v", env.svc.installReq)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("json unmarshal: %v\n%s", err, out)
		}
		if got["engine"] != "clickhouse" || got["version"] != "0.3.0" || got["upgraded"] != true {
			t.Errorf("json = %v", got)
		}
	})

	t.Run("終了コード2と3と4と非rootの1を正しく返す", func(t *testing.T) {
		// 不正な参照 -> 2
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		if code, _, _ := env.run(t, "module", "install", "github.com/acme"); code != 2 {
			t.Errorf("bad ref code = %d, want 2", code)
		}
		// 不正な --trust-key -> 2
		if code, _, _ := env.run(t, "module", "install", "github.com/acme/rt-ch", "--trust-key", "md5:1234"); code != 2 {
			t.Errorf("bad trust-key code = %d, want 2", code)
		}
		// 非 root -> 1
		env.uid = 1000
		if code, _, errOut := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.2.0"); code != 1 || !strings.Contains(errOut, "root") {
			t.Errorf("non-root: code=%d stderr=%q", code, errOut)
		}
		env.uid = 0
		// 指紋不一致 -> 3
		env.svc.installFn = func(context.Context, rtmodule.InstallRequest, rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			return rtmodule.InstallResult{}, fmt.Errorf("%w: mismatch", domain.ErrKeyMismatch)
		}
		if code, _, _ := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.2.0", "--trust-key", testFP2); code != 3 {
			t.Errorf("mismatch code = %d, want 3", code)
		}
		// 既存あり・ダウングレード・出所不一致 -> 4
		for _, domErr := range []error{domain.ErrAlreadyInstalled, domain.ErrDowngradeRefused, rtmodule.ErrSourceMismatch} {
			env.svc.installFn = func(context.Context, rtmodule.InstallRequest, rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
				return rtmodule.InstallResult{}, domErr
			}
			if code, _, _ := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.2.0"); code != 4 {
				t.Errorf("%v: code = %d, want 4", domErr, code)
			}
		}
	})
}

func TestCLI_module_listがクラスタモジュールとローカルランタイムモジュールを表示しjson出力できる(t *testing.T) {
	t.Run("通常table表示はクラスタモジュールとローカルランタイムモジュールの両セクションを表示する", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 1000} // 非root可
		env.api.answers["GET /v1/modules"] = map[string]any{"items": []any{
			map[string]any{"name": "proxysql", "version": "0.2.7", "wanted": true, "nodes": []any{
				map[string]any{"node": "node-1", "loaded": "0.2.7", "current": true},
			}},
		}}
		env.svc.listRes = []rtmodule.ModuleInfo{
			{Engine: "clickhouse", Version: "0.3.0", Signed: true, KeyID: "acme-2026", RunAsUser: "db-branch-rt-clickhouse", Status: "ok"},
			{Engine: "broken", Status: "error", Err: errors.New("bad sig")},
		}

		code, out, errOut := env.run(t, "module", "list")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		for _, want := range []string{
			"CLUSTER MODULES",
			"proxysql",
			"enabled 1/1",
			"RUNTIME MODULES (LOCAL NODE)",
			"ENGINE",
			"clickhouse",
			"0.3.0",
			"yes",
			"acme-2026",
			"db-branch-rt-clickhouse",
			"broken",
			"bad sig",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("json出力は既存配列を維持しruntimeフラグ時のみランタイム配列を返す", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 1000}
		env.api.answers["GET /v1/modules"] = map[string]any{"items": []any{
			map[string]any{"name": "proxysql", "version": "0.2.7", "wanted": true, "nodes": []any{}},
		}}
		env.svc.listRes = []rtmodule.ModuleInfo{
			{Engine: "clickhouse", Version: "0.3.0", Signed: true, KeyID: "acme-2026", RunAsUser: "db-branch-rt-clickhouse", Status: "ok"},
		}

		code, out, errOut := env.run(t, "-o", "json", "module", "list")
		if code != 0 {
			t.Fatalf("json code = %d, stderr = %q", code, errOut)
		}
		var clusterArr []map[string]any
		if err := json.Unmarshal([]byte(out), &clusterArr); err != nil {
			t.Fatalf("unmarshal cluster json: %v\n%s", err, out)
		}
		if len(clusterArr) != 1 || clusterArr[0]["name"] != "proxysql" {
			t.Errorf("cluster json = %v", clusterArr)
		}

		code, out, errOut = env.run(t, "-o", "json", "module", "list", "--runtime")
		if code != 0 {
			t.Fatalf("--runtime json code = %d, stderr = %q", code, errOut)
		}
		var rtArr []map[string]any
		if err := json.Unmarshal([]byte(out), &rtArr); err != nil {
			t.Fatalf("unmarshal runtime json: %v\n%s", err, out)
		}
		if len(rtArr) != 1 || rtArr[0]["engine"] != "clickhouse" || rtArr[0]["key_id"] != "acme-2026" {
			t.Errorf("runtime json = %v", rtArr)
		}
	})

	t.Run("エージェント停止中でもローカルランタイムモジュール一覧を表示できる", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 1000}
		env.api.errors["GET /v1/modules"] = errors.New("cannot reach agent")
		env.svc.listRes = []rtmodule.ModuleInfo{
			{Engine: "clickhouse", Version: "0.3.0", Signed: true, KeyID: "acme-2026", RunAsUser: "db-branch-rt-clickhouse", Status: "ok"},
		}

		code, out, errOut := env.run(t, "module", "list")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !strings.Contains(out, "RUNTIME MODULES (LOCAL NODE)") || !strings.Contains(out, "clickhouse") {
			t.Errorf("stdout = %q", out)
		}
		if !strings.Contains(errOut, "cannot reach agent") {
			t.Errorf("stderr should note agent unreachable: %q", errOut)
		}

		// --runtime の table 出力はエージェントを呼ばない
		code, out, errOut = env.run(t, "module", "list", "--runtime")
		if code != 0 || errOut != "" || !strings.Contains(out, "clickhouse") {
			t.Errorf("--runtime table: code=%d stdout=%q stderr=%q", code, out, errOut)
		}
	})
}

func TestCLI_module_removeとtrustサブコマンドの引数検証とメッセージ出力と終了コード(t *testing.T) {
	fp1, err := domain.ParseFingerprint(testFP1)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("removeはローカル導入済みエンジンを削除しOSユーザーと信頼鍵を残した旨を表示する", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.listRes = []rtmodule.ModuleInfo{{Engine: "clickhouse", Version: "0.3.0", Signed: true, KeyID: "acme-2026", Status: "ok"}}
		env.svc.removeFn = func(_ context.Context, engine string, noRestart bool) (rtmodule.RemoveResult, error) {
			return rtmodule.RemoveResult{
				Engine:    engine,
				Removed:   true,
				Restarted: !noRestart,
				UserKept:  "db-branch-rt-clickhouse",
				KeyIDKept: "acme-2026",
			}, nil
		}

		code, out, errOut := env.run(t, "module", "remove", "clickhouse")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !env.svc.removeArgs.called || env.svc.removeArgs.engine != "clickhouse" || env.svc.removeArgs.noRestart {
			t.Errorf("removeArgs = %+v", env.svc.removeArgs)
		}
		for _, want := range []string{
			"Removed runtime module clickhouse (/usr/lib/db-branch/runtimes/clickhouse).",
			"Restarted db-branch.service.",
			`OS user "db-branch-rt-clickhouse" was NOT deleted`,
			`Trusted key "acme-2026" was kept`,
			"sudo db-branch module trust remove acme-2026",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("removeでローカル未導入なら既存APTモジュール削除へフォールバックする", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 1000}
		env.api.answers["DELETE /v1/modules/proxysql"] = nil

		code, _, errOut := env.run(t, "module", "remove", "proxysql")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if env.svc.removeArgs.called {
			t.Error("should have fallen back to APT API")
		}
	})

	t.Run("removeの使用中は終了コード4で組み込みエンジン指定は終了コード2で非rootは1", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.removeFn = func(context.Context, string, bool) (rtmodule.RemoveResult, error) {
			return rtmodule.RemoveResult{}, fmt.Errorf("%w: engine is in use by 1 base (b1)", domain.ErrInUse)
		}
		if code, _, _ := env.run(t, "module", "remove", "clickhouse", "--runtime"); code != 4 {
			t.Errorf("in-use code = %d, want 4", code)
		}
		if code, _, _ := env.run(t, "module", "remove", "mysql", "--runtime"); code != 2 {
			t.Errorf("built-in engine code = %d, want 2", code)
		}
		env.uid = 1000
		if code, _, errOut := env.run(t, "module", "remove", "clickhouse", "--runtime"); code != 1 || !strings.Contains(errOut, "root") {
			t.Errorf("non-root remove: code=%d stderr=%q", code, errOut)
		}
	})

	t.Run("removeのno-restartとjson出力", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.removeFn = func(_ context.Context, engine string, noRestart bool) (rtmodule.RemoveResult, error) {
			return rtmodule.RemoveResult{Engine: engine, Removed: true, Restarted: !noRestart, UserKept: "db-branch-rt-" + engine}, nil
		}
		code, out, errOut := env.run(t, "-o", "json", "module", "remove", "clickhouse", "--runtime", "--no-restart")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !env.svc.removeArgs.noRestart {
			t.Error("want noRestart=true")
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		if got["engine"] != "clickhouse" || got["removed"] != true || got["user_kept"] != "db-branch-rt-clickhouse" {
			t.Errorf("json = %v", got)
		}
	})

	t.Run("trust addとlistとremoveの入出力と終了コード", func(t *testing.T) {
		pubFile := filepath.Join(t.TempDir(), "acme.pub")
		if err := os.WriteFile(pubFile, []byte("AAAA\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		var gotPub []byte
		var gotExpected domain.Fingerprint
		env.svc.trustAddFn = func(keyID string, pubRaw []byte, expected domain.Fingerprint) (rtmodule.TrustKey, error) {
			gotPub = pubRaw
			gotExpected = expected
			return rtmodule.TrustKey{KeyID: keyID, Fingerprint: fp1}, nil
		}

		// ファイルから trust add
		code, out, errOut := env.run(t, "module", "trust", "add", "acme-2026", pubFile, "--fingerprint", testFP1)
		if code != 0 {
			t.Fatalf("trust add file: code=%d stderr=%q", code, errOut)
		}
		if string(gotPub) != "AAAA\n" || gotExpected.String() != testFP1 {
			t.Errorf("pub=%q fp=%q", gotPub, gotExpected)
		}
		for _, want := range []string{
			`Registered trusted key "acme-2026"`,
			"/etc/db-branch/trusted-keys/acme-2026.pub",
			testFP1,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("trust add stdout lacks %q:\n%s", want, out)
			}
		}

		// stdin ("-") から trust add + json出力
		env.stdin = "BBBB\n"
		code, out, errOut = env.run(t, "-o", "json", "module", "trust", "add", "acme-2026", "-")
		if code != 0 || string(gotPub) != "BBBB\n" || !strings.Contains(out, `"acme-2026"`) {
			t.Errorf("trust add stdin: code=%d pub=%q out=%q err=%q", code, gotPub, out, errOut)
		}

		// 不正な fingerprint -> 2、既存衝突 -> 4、不一致 -> 3、非 root -> 1
		if code, _, _ := env.run(t, "module", "trust", "add", "acme-2026", pubFile, "--fingerprint", "bad"); code != 2 {
			t.Errorf("bad fp code = %d, want 2", code)
		}
		env.svc.trustAddFn = func(string, []byte, domain.Fingerprint) (rtmodule.TrustKey, error) {
			return rtmodule.TrustKey{}, domain.ErrKeyExists
		}
		if code, _, _ := env.run(t, "module", "trust", "add", "acme-2026", pubFile); code != 4 {
			t.Errorf("key exists code = %d, want 4", code)
		}
		env.svc.trustAddFn = func(string, []byte, domain.Fingerprint) (rtmodule.TrustKey, error) {
			return rtmodule.TrustKey{}, domain.ErrKeyMismatch
		}
		if code, _, _ := env.run(t, "module", "trust", "add", "acme-2026", pubFile); code != 3 {
			t.Errorf("key mismatch code = %d, want 3", code)
		}
		env.uid = 1000
		if code, _, errOut := env.run(t, "module", "trust", "add", "acme-2026", pubFile); code != 1 || !strings.Contains(errOut, "root") {
			t.Errorf("non-root trust add: code=%d stderr=%q", code, errOut)
		}

		// trust list は非 root で実行可能 (table と json)
		env.svc.trustListFn = func() ([]rtmodule.TrustKey, error) {
			return []rtmodule.TrustKey{
				{KeyID: "acme-2026", Fingerprint: fp1},
				{KeyID: "bad-key", Err: errors.New("corrupt")},
			}, nil
		}
		code, out, errOut = env.run(t, "module", "trust", "list")
		if code != 0 {
			t.Fatalf("trust list: code=%d stderr=%q", code, errOut)
		}
		for _, want := range []string{"KEY ID", "FINGERPRINT", "STATUS", "acme-2026", testFP1, "ok", "bad-key", "corrupt"} {
			if !strings.Contains(out, want) {
				t.Errorf("trust list lacks %q:\n%s", want, out)
			}
		}
		code, out, errOut = env.run(t, "-o", "json", "module", "trust", "list")
		if code != 0 || !strings.Contains(out, testFP1) || !strings.Contains(out, `"corrupt"`) {
			t.Errorf("trust list json: code=%d out=%q err=%q", code, out, errOut)
		}

		// trust remove: 非 root -> 1、使用中 -> 4、成功 -> 0
		if code, _, errOut := env.run(t, "module", "trust", "remove", "acme-2026"); code != 1 || !strings.Contains(errOut, "root") {
			t.Errorf("non-root trust remove: code=%d stderr=%q", code, errOut)
		}
		env.uid = 0
		env.svc.trustRmFn = func(context.Context, string) error { return domain.ErrInUse }
		if code, _, _ := env.run(t, "module", "trust", "remove", "acme-2026"); code != 4 {
			t.Errorf("trust remove in-use code = %d, want 4", code)
		}
		env.svc.trustRmFn = func(context.Context, string) error { return nil }
		code, out, errOut = env.run(t, "module", "trust", "remove", "acme-2026")
		if code != 0 || !strings.Contains(out, "acme-2026") {
			t.Errorf("trust remove ok: code=%d out=%q err=%q", code, out, errOut)
		}
		code, out, errOut = env.run(t, "-o", "json", "module", "trust", "remove", "acme-2026")
		if code != 0 || !strings.Contains(out, `"removed": true`) {
			t.Errorf("trust remove json: code=%d out=%q err=%q", code, out, errOut)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestCLI_ランタイムモジュールの異常系とフラグ検証と既定関数を網羅する(t *testing.T) {
	fp1, err := domain.ParseFingerprint(testFP1)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("フラグの誤った組み合わせは終了コード2で拒否する", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		for _, args := range [][]string{
			{"module", "install", "github.com/acme/rt-ch", "--version", "1.0"},
			{"module", "install", "proxysql", "--trust-key", testFP1},
			{"module", "install", "proxysql", "--upgrade"},
			{"module", "install", "proxysql", "--no-restart"},
			{"module", "remove", "proxysql", "--no-restart"},
		} {
			if code, _, _ := env.run(t, args...); code != 2 {
				t.Errorf("%v: code = %d, want 2", args, code)
			}
		}
	})

	t.Run("installのタグ省略や既存ユーザーでのupgradeや警告やno-restartと一般エラーを表示できる", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.installFn = func(context.Context, rtmodule.InstallRequest, rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			return rtmodule.InstallResult{
				Engine:          "clickhouse",
				Version:         "0.3.0",
				PreviousVersion: "0.2.0",
				Tag:             "v0.3.0",
				KeyID:           "acme-2026",
				KeyFingerprint:  fp1,
				Upgraded:        true,
				User:            "db-branch-rt-clickhouse",
				UserCreated:     false,
				UserUID:         984,
				UserGID:         984,
				Installed:       true,
				Warnings:        []string{"signing key changed"},
			}, nil
		}

		code, out, errOut := env.run(t, "module", "install", "github.com/acme/rt-ch", "--upgrade", "--no-restart")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut)
		}
		if !strings.Contains(errOut, "Warning: signing key changed") {
			t.Errorf("stderr lacks warning: %q", errOut)
		}
		for _, want := range []string{
			"Fetching latest release from github.com/acme/rt-ch",
			"Verified signature (key_id=acme-2026, trusted)",
			"OS user db-branch-rt-clickhouse already exists (uid=984, gid=984).",
			"Upgraded runtime module clickhouse 0.2.0 -> 0.3.0",
			"Skipped restarting db-branch.service (--no-restart)",
			"--upgrade --trust-key " + testFP1,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}

		// 一般エラー -> 終了コード 1、既に ExitError ならそのまま
		env.svc.installFn = func(context.Context, rtmodule.InstallRequest, rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			return rtmodule.InstallResult{}, errors.New("network failure")
		}
		if code, _, _ := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.3.0"); code != 1 {
			t.Errorf("general err code = %d, want 1", code)
		}
		env.svc.installFn = func(context.Context, rtmodule.InstallRequest, rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			return rtmodule.InstallResult{}, &cli.ExitError{Code: 3, Err: errors.New("custom untrusted")}
		}
		if code, _, _ := env.run(t, "module", "install", "github.com/acme/rt-ch@v0.3.0"); code != 3 {
			t.Errorf("wrapped ExitError code = %d, want 3", code)
		}
	})

	t.Run("removeのno-restartテキスト出力とAPIErrorや未設定フックや不正設定ファイルを処理する", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		env.svc.removeFn = func(context.Context, string, bool) (rtmodule.RemoveResult, error) {
			return rtmodule.RemoveResult{Engine: "clickhouse", Removed: true, UserKept: "db-branch-rt-clickhouse"}, nil
		}
		code, out, errOut := env.run(t, "module", "remove", "clickhouse", "--runtime", "--no-restart")
		if code != 0 || !strings.Contains(out, "Skipped restarting db-branch.service (--no-restart)") {
			t.Errorf("remove no-restart: code=%d out=%q err=%q", code, out, errOut)
		}

		// module list でエージェントが APIError を返したらそのままエラー
		env.api.errors["GET /v1/modules"] = &cli.APIError{Status: 500, Code: "internal", Message: "boom"}
		if code, _, errOut := env.run(t, "module", "list"); code != 1 || !strings.Contains(errOut, "boom") {
			t.Errorf("list APIError: code=%d stderr=%q", code, errOut)
		}
		if code, _, _ := env.run(t, "-o", "json", "module", "list"); code != 1 {
			t.Errorf("list json APIError: code=%d", code)
		}

		// RuntimeModules 未設定時
		badCfg := filepath.Join(t.TempDir(), "bad.toml")
		if err := os.WriteFile(badCfg, []byte("unknown_key = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var outBuf, errBuf bytes.Buffer
		unwired := cli.App{Stdout: &outBuf, Stderr: &errBuf, EffectiveUID: func() int { return 0 }}
		for _, args := range [][]string{
			{"module", "install", "github.com/acme/rt-ch@v0.1.0"},
			{"module", "list", "--runtime"},
			{"module", "remove", "clickhouse", "--runtime"},
			{"module", "trust", "list"},
			{"module", "trust", "remove", "acme-2026"},
		} {
			if code := unwired.Run(context.Background(), args); code == 0 {
				t.Errorf("unwired %v: want non-zero", args)
			}
		}
		// 不正 config.toml
		wired := cli.App{
			Stdout: &outBuf, Stderr: &errBuf, EffectiveUID: func() int { return 0 },
			RuntimeModules: func(config.Config, rtmodule.TrustPrompter) cli.RuntimeModuleService { return env.svc },
		}
		for _, args := range [][]string{
			{"--config", badCfg, "module", "list", "--runtime"},
			{"--config", badCfg, "module", "remove", "clickhouse"},
		} {
			if code := wired.Run(context.Background(), args); code == 0 {
				t.Errorf("bad config %v: want non-zero", args)
			}
		}
	})

	t.Run("trustサブコマンドの入力異常とエラー経路", func(t *testing.T) {
		env := &rtTestEnv{api: newFakeAPI(), svc: &fakeRTModuleService{}, uid: 0}
		// 存在しないファイル
		if code, _, _ := env.run(t, "module", "trust", "add", "acme-2026", filepath.Join(t.TempDir(), "missing.pub")); code != 1 {
			t.Errorf("missing file code = %d, want 1", code)
		}
		// stdin 大きすぎ
		env.stdin = strings.Repeat("A", 5000)
		if code, _, errOut := env.run(t, "module", "trust", "add", "acme-2026", "-"); code != 1 || !strings.Contains(errOut, "exceeds") {
			t.Errorf("oversized stdin: code=%d err=%q", code, errOut)
		}
		// stdin nil / read error / unwired
		var outBuf, errBuf bytes.Buffer
		appNilStdin := cli.App{
			Stdout: &outBuf, Stderr: &errBuf, EffectiveUID: func() int { return 0 },
			RuntimeModules: func(config.Config, rtmodule.TrustPrompter) cli.RuntimeModuleService { return env.svc },
		}
		if code := appNilStdin.Run(context.Background(), []string{"module", "trust", "add", "acme-2026", "-"}); code != 1 {
			t.Errorf("nil stdin code = %d, want 1", code)
		}
		appErrStdin := appNilStdin
		appErrStdin.Stdin = errReader{err: errors.New("read boom")}
		if code := appErrStdin.Run(context.Background(), []string{"module", "trust", "add", "acme-2026", "-"}); code != 1 {
			t.Errorf("err stdin code = %d, want 1", code)
		}
		// trust add unwired
		pubFile := filepath.Join(t.TempDir(), "k.pub")
		_ = os.WriteFile(pubFile, []byte("AAAA\n"), 0o644)
		appUnwired := cli.App{Stdout: &outBuf, Stderr: &errBuf, EffectiveUID: func() int { return 0 }}
		if code := appUnwired.Run(context.Background(), []string{"module", "trust", "add", "acme-2026", pubFile}); code != 1 {
			t.Errorf("trust add unwired code = %d, want 1", code)
		}
		// trust list error
		env.svc.trustListFn = func() ([]rtmodule.TrustKey, error) { return nil, errors.New("list boom") }
		if code, _, _ := env.run(t, "module", "trust", "list"); code != 1 {
			t.Errorf("trust list err code = %d, want 1", code)
		}
		// trust remove invalid key_id -> 2
		env.svc.trustRmFn = func(context.Context, string) error { return domain.ErrInvalidRef }
		if code, _, _ := env.run(t, "module", "trust", "remove", "../bad"); code != 2 {
			t.Errorf("trust remove bad key_id code = %d, want 2", code)
		}
	})

	t.Run("対話プロンプトのキャンセルとStdin異常と既定IsTTYと既定EffectiveUID", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "c.toml")
		// 既定の IsTTY (非ファイル / 通常ファイル -> false)
		svc := &fakeRTModuleService{}
		svc.installFn = func(ctx context.Context, req rtmodule.InstallRequest, p rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			_, err := p.ConfirmTrust(ctx, "acme-2026", fp1, req.Ref, "v0.1.0")
			return rtmodule.InstallResult{}, err
		}
		var outBuf, errBuf bytes.Buffer
		app := cli.App{
			Stdin: strings.NewReader("y\n"), Stdout: &outBuf, Stderr: &errBuf,
			EffectiveUID: func() int { return 0 },
			RuntimeModules: func(_ config.Config, p rtmodule.TrustPrompter) cli.RuntimeModuleService {
				svc.prompter = p
				return svc
			},
		}
		if code := app.Run(context.Background(), []string{"--config", cfgPath, "module", "install", "github.com/acme/rt-ch@v0.1.0"}); code != 3 {
			t.Errorf("default IsTTY non-file code = %d, want 3", code)
		}
		// 通常ファイル *os.File を Stdin に渡しても IsTTY は false
		regFile, err := os.CreateTemp(t.TempDir(), "in")
		if err != nil {
			t.Fatal(err)
		}
		defer regFile.Close()
		app.Stdin = regFile
		if code := app.Run(context.Background(), []string{"--config", cfgPath, "module", "install", "github.com/acme/rt-ch@v0.1.0"}); code != 3 {
			t.Errorf("default IsTTY regular file code = %d, want 3", code)
		}
		// TTY=true で Stdin=nil -> false (declined)
		app.IsTTY = func(io.Reader) bool { return true }
		app.Stdin = nil
		svc.installFn = func(ctx context.Context, req rtmodule.InstallRequest, p rtmodule.TrustPrompter) (rtmodule.InstallResult, error) {
			ok, err := p.ConfirmTrust(ctx, "acme-2026", fp1, req.Ref, "v0.1.0")
			if err != nil {
				return rtmodule.InstallResult{}, err
			}
			if !ok {
				return rtmodule.InstallResult{}, domain.ErrKeyUntrusted
			}
			return rtmodule.InstallResult{}, nil
		}
		if code := app.Run(context.Background(), []string{"--config", cfgPath, "module", "install", "github.com/acme/rt-ch@v0.1.0"}); code != 3 {
			t.Errorf("TTY nil stdin code = %d, want 3", code)
		}
		// TTY=true で Read エラー
		app.Stdin = errReader{err: errors.New("tty read err")}
		if code := app.Run(context.Background(), []string{"--config", cfgPath, "module", "install", "github.com/acme/rt-ch@v0.1.0"}); code != 1 {
			t.Errorf("TTY read err code = %d, want 1", code)
		}
		// コンテキストキャンセル済み
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if code := app.Run(cancelledCtx, []string{"--config", cfgPath, "module", "install", "github.com/acme/rt-ch@v0.1.0"}); code != 1 {
			t.Errorf("cancelled ctx code = %d, want 1", code)
		}
		// 既定 EffectiveUID (非 root 環境なら root エラー、root なら次へ進む)
		appDefUID := cli.App{
			Stdout: &outBuf, Stderr: &errBuf,
			RuntimeModules: func(config.Config, rtmodule.TrustPrompter) cli.RuntimeModuleService { return svc },
		}
		code := appDefUID.Run(context.Background(), []string{"--config", cfgPath, "module", "trust", "remove", "acme-2026"})
		if os.Geteuid() != 0 && code != 1 {
			t.Errorf("default euid non-root code = %d, want 1", code)
		}
	})
}

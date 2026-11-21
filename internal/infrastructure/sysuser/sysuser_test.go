package sysuser_test

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/sysuser"
)

type recordedCmd struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []recordedCmd
	onRun func(name string, args ...string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, recordedCmd{name: name, args: append([]string(nil), args...)})
	if f.onRun != nil {
		return f.onRun(name, args...)
	}
	return nil, nil
}

func TestEnsureSystemUser_未作成ならログイン不可ホームなしのシステムユーザーとグループを作成する(t *testing.T) {
	const username = "db-branch-rt-clickhouse"
	var userCreated, groupCreated bool

	runner := &fakeRunner{
		onRun: func(name string, _ ...string) ([]byte, error) {
			switch name {
			case "groupadd":
				groupCreated = true
			case "useradd":
				userCreated = true
			}
			return nil, nil
		},
	}

	u := &sysuser.Users{
		Run: runner,
		Lookup: func(name string) (*user.User, error) {
			if userCreated && name == username {
				return &user.User{
					Username: username,
					Uid:      "984",
					Gid:      "984",
					HomeDir:  "/nonexistent",
				}, nil
			}
			return nil, user.UnknownUserError(name)
		},
		LookupGroup: func(name string) (*user.Group, error) {
			if groupCreated && name == username {
				return &user.Group{Name: username, Gid: "984"}, nil
			}
			return nil, user.UnknownGroupError(name)
		},
	}

	created, uid, gid, err := u.EnsureSystemUser(context.Background(), username)
	if err != nil {
		t.Fatalf("EnsureSystemUser() error = %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	if uid != 984 || gid != 984 {
		t.Errorf("uid, gid = %d, %d, want 984, 984", uid, gid)
	}

	wantCalls := []recordedCmd{
		{name: "groupadd", args: []string{"--system", username}},
		{name: "useradd", args: []string{"--system", "--gid", username, "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", username}},
	}
	if len(runner.calls) != len(wantCalls) {
		t.Fatalf("calls = %+v, want %+v", runner.calls, wantCalls)
	}
	for i, want := range wantCalls {
		got := runner.calls[i]
		if got.name != want.name || !slices.Equal(got.args, want.args) {
			t.Errorf("call[%d] = %s %v, want %s %v", i, got.name, got.args, want.name, want.args)
		}
	}
}

func TestEnsureSystemUser_既存の安全なシステムユーザーなら冪等に成功しuid0やログイン可能ユーザーは拒否する(t *testing.T) {
	const username = "db-branch-rt-clickhouse"

	// 1. 既存の安全なシステムユーザー（nologin / false の各標準パス）は冪等に成功する
	for _, shell := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/false", "/bin/false"} {
		passwd := filepath.Join(t.TempDir(), "passwd")
		content := "# comment\nroot:x:0:0:root:/root:/bin/bash\n" +
			username + ":x:980:980::/nonexistent:" + shell + "\n"
		if err := os.WriteFile(passwd, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		u := &sysuser.Users{
			Run:        runner,
			PasswdPath: passwd,
			Lookup: func(string) (*user.User, error) {
				return &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"}, nil
			},
		}
		created, uid, gid, err := u.EnsureSystemUser(context.Background(), username)
		if err != nil {
			t.Fatalf("shell %s: EnsureSystemUser() error = %v", shell, err)
		}
		if created {
			t.Errorf("shell %s: created = true, want false", shell)
		}
		if uid != 980 || gid != 980 {
			t.Errorf("shell %s: uid, gid = %d, %d, want 980, 980", shell, uid, gid)
		}
		if len(runner.calls) != 0 {
			t.Errorf("shell %s: unexpected commands run: %+v", shell, runner.calls)
		}
	}

	// 2. 既存ユーザーが危険な設定（uid 0 / gid 0 / ログイン可能シェル / /nonexistent 以外のホーム）なら理由付きで拒否
	unsafeCases := []struct {
		name       string
		usr        *user.User
		shell      string
		wantReason string
	}{
		{
			name:       "uid 0",
			usr:        &user.User{Username: username, Uid: "0", Gid: "980", HomeDir: "/nonexistent"},
			shell:      "/usr/sbin/nologin",
			wantReason: "uid 0",
		},
		{
			name:       "gid 0",
			usr:        &user.User{Username: username, Uid: "980", Gid: "0", HomeDir: "/nonexistent"},
			shell:      "/usr/sbin/nologin",
			wantReason: "gid 0",
		},
		{
			name:       "ログイン可能シェル /bin/bash",
			usr:        &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"},
			shell:      "/bin/bash",
			wantReason: "shell",
		},
		{
			name:       "空シェル",
			usr:        &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"},
			shell:      "",
			wantReason: "shell",
		},
		{
			name:       "ホームディレクトリが /nonexistent 以外",
			usr:        &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/home/clickhouse"},
			shell:      "/usr/sbin/nologin",
			wantReason: "home",
		},
	}
	for _, tc := range unsafeCases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			u := &sysuser.Users{
				Run:         runner,
				Lookup:      func(string) (*user.User, error) { return tc.usr, nil },
				LookupShell: func(string) (string, error) { return tc.shell, nil },
			}
			_, _, _, err := u.EnsureSystemUser(context.Background(), username)
			if !errors.Is(err, sysuser.ErrUnsafeUser) {
				t.Fatalf("err = %v, want ErrUnsafeUser", err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantReason) {
				t.Errorf("err = %q, want reason containing %q", err.Error(), tc.wantReason)
			}
			if len(runner.calls) != 0 {
				t.Errorf("unexpected commands run: %+v", runner.calls)
			}
		})
	}

	// 3. db-branch-rt- 以外のユーザー名や不正エンジン名は拒否
	for _, bad := range []string{
		"",
		"root",
		"db-branch",
		"clickhouse",
		"db-branch-rt-",
		"db-branch-rt-mysql",
		"db-branch-rt-postgres",
		"db-branch-rt-../evil",
		"db-branch-rt-Upper",
		"db-branch-rt-aaaaaaaaaaaaaaaaaaaa", // 20文字 (> 19文字上限)
	} {
		runner := &fakeRunner{}
		u := &sysuser.Users{Run: runner}
		if _, _, _, err := u.EnsureSystemUser(context.Background(), bad); !errors.Is(err, domain.ErrInvalidModule) {
			t.Errorf("EnsureSystemUser(%q) err = %v, want ErrInvalidModule", bad, err)
		}
		if len(runner.calls) != 0 {
			t.Errorf("EnsureSystemUser(%q) ran commands: %+v", bad, runner.calls)
		}
	}
}

func TestEnsureSystemUser_既存グループ再利用と各種障害や異常IDを安全に処理する(t *testing.T) {
	const username = "db-branch-rt-clickhouse"

	t.Run("安全な既存グループがあればgroupaddを省略してuseraddだけ実行する", func(t *testing.T) {
		var userCreated bool
		runner := &fakeRunner{
			onRun: func(name string, _ ...string) ([]byte, error) {
				if name == "useradd" {
					userCreated = true
				}
				return nil, nil
			},
		}
		u := &sysuser.Users{
			Run: runner,
			Lookup: func(name string) (*user.User, error) {
				if userCreated {
					return &user.User{Username: name, Uid: "981", Gid: "980", HomeDir: "/nonexistent"}, nil
				}
				return nil, user.UnknownUserError(name)
			},
			LookupGroup: func(name string) (*user.Group, error) {
				return &user.Group{Name: name, Gid: "980"}, nil
			},
		}
		created, uid, gid, err := u.EnsureSystemUser(context.Background(), username)
		if err != nil {
			t.Fatalf("EnsureSystemUser() error = %v", err)
		}
		if !created || uid != 981 || gid != 980 {
			t.Errorf("got (%v, %d, %d), want (true, 981, 980)", created, uid, gid)
		}
		if len(runner.calls) != 1 || runner.calls[0].name != "useradd" {
			t.Errorf("calls = %+v, want only useradd", runner.calls)
		}
	})

	t.Run("既存グループのgidが0または数値以外なら拒否する", func(t *testing.T) {
		for _, badGid := range []string{"0", "notanumber"} {
			runner := &fakeRunner{}
			u := &sysuser.Users{
				Run:         runner,
				Lookup:      func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) },
				LookupGroup: func(name string) (*user.Group, error) { return &user.Group{Name: name, Gid: badGid}, nil },
			}
			if _, _, _, err := u.EnsureSystemUser(context.Background(), username); !errors.Is(err, sysuser.ErrUnsafeUser) {
				t.Errorf("gid=%q: err = %v, want ErrUnsafeUser", badGid, err)
			}
			if len(runner.calls) != 0 {
				t.Errorf("gid=%q: unexpected commands %+v", badGid, runner.calls)
			}
		}
	})

	t.Run("LookupやLookupGroupの予期しないエラーやコマンド失敗を返す", func(t *testing.T) {
		ioErr := errors.New("nss error")
		uLookupErr := &sysuser.Users{
			Run:    &fakeRunner{},
			Lookup: func(string) (*user.User, error) { return nil, ioErr },
		}
		if _, _, _, err := uLookupErr.EnsureSystemUser(context.Background(), username); !errors.Is(err, ioErr) {
			t.Errorf("Lookup error = %v, want %v", err, ioErr)
		}

		uGroupLookupErr := &sysuser.Users{
			Run:         &fakeRunner{},
			Lookup:      func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) },
			LookupGroup: func(string) (*user.Group, error) { return nil, ioErr },
		}
		if _, _, _, err := uGroupLookupErr.EnsureSystemUser(context.Background(), username); !errors.Is(err, ioErr) {
			t.Errorf("LookupGroup error = %v, want %v", err, ioErr)
		}

		cmdErr := errors.New("exit status 9")
		groupAddFail := &fakeRunner{onRun: func(name string, _ ...string) ([]byte, error) {
			if name == "groupadd" {
				return nil, cmdErr
			}
			return nil, nil
		}}
		uGroupFail := &sysuser.Users{
			Run:         groupAddFail,
			Lookup:      func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) },
			LookupGroup: func(name string) (*user.Group, error) { return nil, user.UnknownGroupError(name) },
		}
		if _, _, _, err := uGroupFail.EnsureSystemUser(context.Background(), username); !errors.Is(err, cmdErr) {
			t.Errorf("groupadd error = %v, want %v", err, cmdErr)
		}
		if len(groupAddFail.calls) != 1 {
			t.Errorf("useradd should not run after groupadd failure: %+v", groupAddFail.calls)
		}

		userAddFail := &fakeRunner{onRun: func(name string, _ ...string) ([]byte, error) {
			if name == "useradd" {
				return nil, cmdErr
			}
			return nil, nil
		}}
		uUserFail := &sysuser.Users{
			Run:         userAddFail,
			Lookup:      func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) },
			LookupGroup: func(name string) (*user.Group, error) { return nil, user.UnknownGroupError(name) },
		}
		if _, _, _, err := uUserFail.EnsureSystemUser(context.Background(), username); !errors.Is(err, cmdErr) {
			t.Errorf("useradd error = %v, want %v", err, cmdErr)
		}
	})

	t.Run("作成後の再Lookup失敗やuid0や数値以外IDを拒否する", func(t *testing.T) {
		// 作成後もユーザーが見つからない
		uMissing := &sysuser.Users{
			Run:         &fakeRunner{},
			Lookup:      func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) },
			LookupGroup: func(name string) (*user.Group, error) { return nil, user.UnknownGroupError(name) },
		}
		if _, _, _, err := uMissing.EnsureSystemUser(context.Background(), username); err == nil {
			t.Error("missing user after useradd was accepted")
		}

		// 作成後に uid=0 や非数値 uid/gid が返った場合は拒否
		for _, bad := range []*user.User{
			{Username: username, Uid: "0", Gid: "980", HomeDir: "/nonexistent"},
			{Username: username, Uid: "980", Gid: "0", HomeDir: "/nonexistent"},
			{Username: username, Uid: "bad", Gid: "980", HomeDir: "/nonexistent"},
			{Username: username, Uid: "980", Gid: "bad", HomeDir: "/nonexistent"},
		} {
			calls := 0
			uBad := &sysuser.Users{
				Run: &fakeRunner{},
				Lookup: func(name string) (*user.User, error) {
					calls++
					if calls == 1 {
						return nil, user.UnknownUserError(name)
					}
					return bad, nil
				},
				LookupGroup: func(name string) (*user.Group, error) { return nil, user.UnknownGroupError(name) },
			}
			if _, _, _, err := uBad.EnsureSystemUser(context.Background(), username); !errors.Is(err, sysuser.ErrUnsafeUser) {
				t.Errorf("post-create %+v: err = %v, want ErrUnsafeUser", bad, err)
			}
		}
	})

	t.Run("passwdの欠損・破損行・未登録ユーザーと既定フィールドを安全に扱う", func(t *testing.T) {
		// 存在しない passwd ファイル
		uNoFile := &sysuser.Users{
			PasswdPath: filepath.Join(t.TempDir(), "missing"),
			Lookup: func(string) (*user.User, error) {
				return &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"}, nil
			},
		}
		if _, _, _, err := uNoFile.EnsureSystemUser(context.Background(), username); !errors.Is(err, sysuser.ErrUnsafeUser) {
			t.Errorf("missing passwd err = %v, want ErrUnsafeUser", err)
		}

		// 破損行と対象ユーザー不在の passwd ファイル
		passwd := filepath.Join(t.TempDir(), "passwd")
		if err := os.WriteFile(passwd, []byte("\nmalformed:line\nother:x:1000:1000::/home/other:/bin/bash\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		uNotInPasswd := &sysuser.Users{
			PasswdPath: passwd,
			Lookup: func(string) (*user.User, error) {
				return &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"}, nil
			},
		}
		if _, _, _, err := uNotInPasswd.EnsureSystemUser(context.Background(), username); !errors.Is(err, sysuser.ErrUnsafeUser) {
			t.Errorf("user missing from passwd err = %v, want ErrUnsafeUser", err)
		}

		// 既定の PasswdPath (/etc/passwd) 読取と既定 Lookup / LookupGroup のフォールバック（コマンドはフェイクで止める）
		uDefaultPasswd := &sysuser.Users{
			Lookup: func(string) (*user.User, error) {
				return &user.User{Username: username, Uid: "980", Gid: "980", HomeDir: "/nonexistent"}, nil
			},
		}
		if _, _, _, err := uDefaultPasswd.EnsureSystemUser(context.Background(), username); !errors.Is(err, sysuser.ErrUnsafeUser) {
			t.Errorf("default /etc/passwd err = %v, want ErrUnsafeUser", err)
		}

		stopErr := errors.New("fake stop")
		uDefaultLookups := &sysuser.Users{
			Run: &fakeRunner{onRun: func(string, ...string) ([]byte, error) { return nil, stopErr }},
		}
		if _, _, _, err := uDefaultLookups.EnsureSystemUser(context.Background(), "db-branch-rt-nonexistent"); !errors.Is(err, stopErr) {
			t.Errorf("default lookups err = %v, want %v", err, stopErr)
		}
	})
}

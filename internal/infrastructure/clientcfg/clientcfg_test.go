package clientcfg_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
)

func TestValidateProfileName_英数字と記号のみ許可する(t *testing.T) {
	for _, ok := range []string{"default", "prod-1", "staging_eu.west"} {
		if err := clientcfg.ValidateProfileName(ok); err != nil {
			t.Errorf("ValidateProfileName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a/b", `a"b`, "a\nb", "../x", strings.Repeat("a", 65)} {
		if err := clientcfg.ValidateProfileName(bad); err == nil {
			t.Errorf("ValidateProfileName(%q) = nil, want error", bad)
		}
	}
}

func TestPathForOS_OSごとの設定ファイルパスを返す(t *testing.T) {
	home := func() (string, error) { return "/Users/alice", nil }
	env := func(map[string]string) func(string) string {
		return func(k string) string { return map[string]string{}[k] }
	}

	got, err := clientcfg.PathForOS("darwin", env(nil), home)
	if err != nil || got != filepath.Join("/Users/alice", ".config", "db-branch", "client.toml") {
		t.Fatalf("darwin default = (%q, %v)", got, err)
	}

	got, err = clientcfg.PathForOS("linux", func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}, home)
	if err != nil || got != filepath.Join("/xdg", "db-branch", "client.toml") {
		t.Fatalf("linux xdg = (%q, %v)", got, err)
	}

	got, err = clientcfg.PathForOS("windows", func(k string) string {
		if k == "AppData" {
			return `C:\Users\alice\AppData\Roaming`
		}
		return ""
	}, home)
	if err != nil || got != filepath.Join(`C:\Users\alice\AppData\Roaming`, "db-branch", "client.toml") {
		t.Fatalf("windows appdata = (%q, %v)", got, err)
	}

	got, err = clientcfg.PathForOS("windows", env(nil), home)
	if err != nil || !strings.HasSuffix(got, filepath.Join("AppData", "Roaming", "db-branch", "client.toml")) {
		t.Fatalf("windows fallback = (%q, %v)", got, err)
	}

	if creds := clientcfg.CredentialsPath("/tmp/custom/client.toml"); creds != filepath.Join("/tmp/custom", "credentials.toml") {
		t.Fatalf("CredentialsPath = %q", creds)
	}
}

func TestLoadAndSave_設定を保存して読み戻せる(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "client.toml")

	cfg, err := clientcfg.Load(path)
	if err != nil {
		t.Fatalf("missing file Load = %v", err)
	}
	if cfg.CurrentProfile != "" || len(cfg.Profiles) != 0 {
		t.Fatalf("missing file cfg = %+v", cfg)
	}

	cfg.CurrentProfile = "prod"
	cfg.Profiles["prod"] = clientcfg.Profile{
		URL:          "https://db.example.com:7080",
		TLSSHA256:    "abcd",
		TokenStorage: "keychain",
	}
	if err := clientcfg.Save(path, cfg); err != nil {
		t.Fatalf("Save = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}

	loaded, err := clientcfg.Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if loaded.CurrentProfile != "prod" || loaded.Profiles["prod"].URL != "https://db.example.com:7080" || loaded.Profiles["prod"].TLSSHA256 != "abcd" {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestLoad_未知キーや不正プロファイル名を拒否する(t *testing.T) {
	dir := t.TempDir()
	badKey := filepath.Join(dir, "badkey.toml")
	if err := os.WriteFile(badKey, []byte("unknown = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientcfg.Load(badKey); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown key err = %v", err)
	}

	badTOML := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(badTOML, []byte("not valid toml =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientcfg.Load(badTOML); err == nil {
		t.Error("invalid toml should fail")
	}

	badProf := filepath.Join(dir, "badprof.toml")
	if err := os.WriteFile(badProf, []byte("[profiles.\"bad/name\"]\nurl = \"https://x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientcfg.Load(badProf); err == nil {
		t.Error("invalid profile name should fail")
	}
	if _, err := clientcfg.Load(dir); err == nil {
		t.Error("Load on directory should fail")
	}
	if _, err := clientcfg.Load(func() string {
		p := filepath.Join(dir, "emptyprof.toml")
		_ = os.WriteFile(p, []byte("current_profile = \"default\"\n"), 0o600)
		return p
	}()); err != nil {
		t.Errorf("valid config without [profiles] should succeed: %v", err)
	}
}

func TestDefaultPathAndErrors_パス解決と保存エラー(t *testing.T) {
	if p, err := clientcfg.DefaultPath(); err != nil || p == "" {
		t.Errorf("DefaultPath = (%q, %v)", p, err)
	}
	if p, err := clientcfg.ResolvePath("/tmp/x.toml"); err != nil || p != "/tmp/x.toml" {
		t.Errorf("ResolvePath(explicit) = (%q, %v)", p, err)
	}
	if p, err := clientcfg.ResolvePath(""); err != nil || p == "" {
		t.Errorf("ResolvePath(empty) = (%q, %v)", p, err)
	}
	badHome := func() (string, error) { return "", os.ErrNotExist }
	emptyEnv := func(string) string { return "" }
	if _, err := clientcfg.PathForOS("linux", emptyEnv, badHome); err == nil {
		t.Error("linux home error should fail")
	}
	if _, err := clientcfg.PathForOS("windows", emptyEnv, badHome); err == nil {
		t.Error("windows home error should fail")
	}

	dir := t.TempDir()
	badCfg := clientcfg.Config{Profiles: map[string]clientcfg.Profile{".bad": {URL: "https://x"}}}
	if err := clientcfg.Save(filepath.Join(dir, "client.toml"), badCfg); err == nil {
		t.Error("Save with invalid profile name should fail")
	}

	// Target path is a directory -> os.Rename fails.
	if err := clientcfg.Save(dir, clientcfg.Config{Profiles: map[string]clientcfg.Profile{}}); err == nil {
		t.Error("Save over directory should fail")
	}

	// Parent path is a regular file -> MkdirAll fails.
	fileParent := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clientcfg.Save(filepath.Join(fileParent, "client.toml"), clientcfg.Config{}); err == nil {
		t.Error("Save under file parent should fail")
	}
	if os.Geteuid() != 0 {
		roDir := filepath.Join(dir, "ro")
		if err := os.Mkdir(roDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
		if err := clientcfg.WriteFileAtomic(filepath.Join(roDir, "f"), []byte("x")); err == nil {
			t.Error("WriteFileAtomic in read-only dir should fail")
		}
	}

	var cfg clientcfg.Config
	if err := cfg.SetProfile("bad/name", clientcfg.Profile{}, true); err == nil {
		t.Error("SetProfile invalid name should fail")
	}
	if err := cfg.SetProfile("prod", clientcfg.Profile{URL: "https://a"}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentProfile != "prod" {
		t.Errorf("CurrentProfile = %q, want prod", cfg.CurrentProfile)
	}
	if err := cfg.SetProfile("dev", clientcfg.Profile{URL: "https://b"}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentProfile != "prod" {
		t.Errorf("CurrentProfile should remain prod, got %q", cfg.CurrentProfile)
	}
	if err := cfg.UseProfile("bad/name"); err == nil {
		t.Error("UseProfile invalid should fail")
	}
	if err := cfg.UseProfile("missing"); err == nil {
		t.Error("UseProfile missing should fail")
	}
	if err := cfg.UseProfile("dev"); err != nil || cfg.CurrentProfile != "dev" {
		t.Errorf("UseProfile dev = (%q, %v)", cfg.CurrentProfile, err)
	}
	if err := cfg.RemoveProfile("bad/name"); err == nil {
		t.Error("RemoveProfile invalid should fail")
	}
	if err := cfg.RemoveProfile("missing"); err == nil {
		t.Error("RemoveProfile missing should fail")
	}
	if err := cfg.RemoveProfile("dev"); err != nil || cfg.CurrentProfile != "" {
		t.Errorf("RemoveProfile dev = (%q, %v)", cfg.CurrentProfile, err)
	}
}


package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildFakeClientTarGz(t *testing.T, scriptBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte(scriptBody)
	if err := tw.WriteHeader(&tar.Header{
		Name: "db-branch",
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallClientScript_ダウンロード検証と配置が成功する(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer test runs on Unix")
	}

	tarGz := buildFakeClientTarGz(t, "#!/usr/bin/env sh\necho \"db-branch 9.9.9\"\n")
	sum := fmt.Sprintf("%x", sha256.Sum256(tarGz))
	archiveName := fmt.Sprintf("db-branch_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sumsBody := fmt.Sprintf("%s  %s\n0000000000000000000000000000000000000000000000000000000000000000  db-branch_9.9.9_windows_amd64.zip\n", sum, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/download/SHA256SUMS", "/download/v9.9.9/SHA256SUMS":
			_, _ = w.Write([]byte(sumsBody))
		case "/latest/download/" + archiveName, "/download/v9.9.9/" + archiveName:
			_, _ = w.Write(tarGz)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	for _, ver := range []string{"", "v9.9.9"} {
		t.Run("version="+ver, func(t *testing.T) {
			installDir := filepath.Join(t.TempDir(), "bin")
			cmd := exec.Command("sh", filepath.Join("install-client.sh"))
			cmd.Env = append(os.Environ(),
				"DB_BRANCH_RELEASE_BASE="+srv.URL,
				"DB_BRANCH_INSTALL_DIR="+installDir,
				"DB_BRANCH_VERSION="+ver,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install-client.sh failed: %v\n%s", err, out)
			}
			got, err := exec.Command(filepath.Join(installDir, "db-branch"), "version").CombinedOutput()
			if err != nil {
				t.Fatalf("installed binary failed: %v\n%s", err, got)
			}
			if !strings.Contains(string(got), "db-branch 9.9.9") {
				t.Fatalf("unexpected version output: %q", string(got))
			}
		})
	}
}

func TestInstallClientScript_SHA256不一致時は失敗し配置しない(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer test runs on Unix")
	}

	tarGz := buildFakeClientTarGz(t, "#!/usr/bin/env sh\necho tampered\n")
	archiveName := fmt.Sprintf("db-branch_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	badSum := strings.Repeat("a", 64)
	sumsBody := fmt.Sprintf("%s  %s\n", badSum, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/download/SHA256SUMS":
			_, _ = w.Write([]byte(sumsBody))
		case "/latest/download/" + archiveName:
			_, _ = w.Write(tarGz)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	installDir := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("sh", filepath.Join("install-client.sh"))
	cmd.Env = append(os.Environ(),
		"DB_BRANCH_RELEASE_BASE="+srv.URL,
		"DB_BRANCH_INSTALL_DIR="+installDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure on SHA-256 mismatch, succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "SHA-256 mismatch") {
		t.Fatalf("expected SHA-256 mismatch error, got:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "db-branch")); !os.IsNotExist(statErr) {
		t.Fatalf("binary must not be installed when checksum fails")
	}
}

func TestInstallClientScript_不正入力や危険なファイル名を拒否する(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer test runs on Unix")
	}

	archiveSuffix := fmt.Sprintf("_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/download/SHA256SUMS" {
			switch r.Header.Get("User-Agent") {
			default:
				_, _ = fmt.Fprintf(w, "%s  db-branch_../evil%s\n", strings.Repeat("b", 64), archiveSuffix)
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Run("unsafe version", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", filepath.Join("install-client.sh"))
		cmd.Env = append(os.Environ(),
			"DB_BRANCH_RELEASE_BASE="+srv.URL,
			"DB_BRANCH_INSTALL_DIR="+installDir,
			"DB_BRANCH_VERSION=v1.0.0;rm -rf /",
		)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "invalid DB_BRANCH_VERSION") {
			t.Fatalf("expected invalid DB_BRANCH_VERSION error, got err=%v out=%s", err, out)
		}
	})

	t.Run("unsafe archive name in SHA256SUMS", func(t *testing.T) {
		installDir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", filepath.Join("install-client.sh"))
		cmd.Env = append(os.Environ(),
			"DB_BRANCH_RELEASE_BASE="+srv.URL,
			"DB_BRANCH_INSTALL_DIR="+installDir,
		)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unsafe archive filename") {
			t.Fatalf("expected unsafe archive filename error, got err=%v out=%s", err, out)
		}
		if _, statErr := os.Stat(filepath.Join(installDir, "db-branch")); !os.IsNotExist(statErr) {
			t.Fatalf("binary must not be installed for unsafe filename")
		}
	})

	t.Run("query char in archive name rejected", func(t *testing.T) {
		badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "%s  db-branch_1.0.0?evil=1%s\n", strings.Repeat("c", 64), archiveSuffix)
		}))
		defer badSrv.Close()
		installDir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", filepath.Join("install-client.sh"))
		cmd.Env = append(os.Environ(),
			"DB_BRANCH_RELEASE_BASE="+badSrv.URL,
			"DB_BRANCH_INSTALL_DIR="+installDir,
		)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unsafe archive filename") {
			t.Fatalf("expected unsafe archive filename error, got err=%v out=%s", err, out)
		}
	})
}

func TestInstallClientScripts_READMEとdocsのインストール手順と整合する(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	clientDoc, err := os.ReadFile(filepath.Join("..", "docs", "client-cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	ps1, err := os.ReadFile(filepath.Join("install-client.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ps1), "SHA256SUMS") || !strings.Contains(string(ps1), "Get-FileHash") {
		t.Errorf("install-client.ps1 must verify SHA256SUMS using Get-FileHash")
	}

	shCmd := "curl -fsSL https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.sh | sh"
	psCmd := "irm https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.ps1 | iex"
	for name, content := range map[string]string{
		"README.md":          string(readme),
		"docs/client-cli.md": string(clientDoc),
	} {
		if !strings.Contains(content, shCmd) {
			t.Errorf("%s missing POSIX client install one-liner", name)
		}
		if !strings.Contains(content, psCmd) {
			t.Errorf("%s missing PowerShell client install one-liner", name)
		}
		if !strings.Contains(content, "brew install db-branch/tap/db-branch") {
			t.Errorf("%s missing Homebrew install command", name)
		}
	}
}

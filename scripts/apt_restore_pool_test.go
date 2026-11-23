package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func runAptRestorePool(t *testing.T, url, outdir string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(".", "apt-restore-pool.sh"), url, outdir)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAptRestorePool_404のみ空として継続しUAを付与する(t *testing.T) {
	var seenUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		http.NotFound(w, r)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	out, err := runAptRestorePool(t, srv.URL, outDir, "APT_RESTORE_RETRY=0")
	if err != nil {
		t.Fatalf("expected 404 to succeed, got err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "restored 0 files") {
		t.Errorf("unexpected output: %s", out)
	}
	ua, _ := seenUA.Load().(string)
	if !strings.Contains(ua, "db-branch-publish/") {
		t.Errorf("User-Agent = %q, want db-branch-publish/*", ua)
	}
}

func TestAptRestorePool_5xxや403や接続不可は非0終了する(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"500", http.StatusInternalServerError},
		{"503", http.StatusServiceUnavailable},
		{"403", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "binary-amd64/Packages") {
					w.WriteHeader(tc.status)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			out, err := runAptRestorePool(t, srv.URL, t.TempDir(), "APT_RESTORE_RETRY=0")
			if err == nil {
				t.Fatalf("status %d: expected non-zero exit, got 0\n%s", tc.status, out)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NewServeMux())
		url := srv.URL
		srv.Close()

		out, err := runAptRestorePool(t, url, t.TempDir(), "APT_RESTORE_RETRY=0")
		if err == nil {
			t.Fatalf("unreachable server: expected non-zero exit, got 0\n%s", out)
		}
	})
}

func TestAptRestorePool_200でプール取得とリトライとSHA256検証をする(t *testing.T) {
	payload := []byte("fake-deb-payload-v1\n")
	sumBytes := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sumBytes[:])
	pkgRel := "pool/main/d/db-branch/db-branch_1.0.0_amd64.deb"

	var idxHits atomic.Int32
	var pkgHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/stable/main/binary-amd64/Packages":
			if idxHits.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, "Package: db-branch\nVersion: 1.0.0\nSHA256: %s\nFilename: %s\n\n", sumHex, pkgRel)
		case "/dists/stable/main/binary-arm64/Packages":
			http.NotFound(w, r)
		case "/" + pkgRel:
			if pkgHits.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	out, err := runAptRestorePool(t, srv.URL+"/", outDir, "APT_RESTORE_RETRY=1", "APT_RESTORE_RETRY_DELAY=0")
	if err != nil {
		t.Fatalf("restore failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restored 1 files") {
		t.Errorf("output = %q, want restored 1 files", out)
	}
	got, err := os.ReadFile(filepath.Join(outDir, pkgRel))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: %q", got)
	}

	// Second run skips already-verified files.
	out2, err := runAptRestorePool(t, srv.URL, outDir, "APT_RESTORE_RETRY=0")
	if err != nil {
		t.Fatalf("second restore failed: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "restored 0 files") {
		t.Errorf("second output = %q, want restored 0 files", out2)
	}

	// Corrupt SHA256 fails without leaving destination file.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/stable/main/binary-amd64/Packages":
			fmt.Fprintf(w, "Package: db-branch\nVersion: 1.0.0\nFilename: %s\nSHA256: %s\n\n", pkgRel, strings.Repeat("0", 64))
		case "/dists/stable/main/binary-arm64/Packages":
			http.NotFound(w, r)
		case "/" + pkgRel:
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer badSrv.Close()

	badDir := t.TempDir()
	if out3, err := runAptRestorePool(t, badSrv.URL, badDir, "APT_RESTORE_RETRY=0"); err == nil {
		t.Fatalf("corrupt sha256: expected failure, got 0\n%s", out3)
	}
	if _, err := os.Stat(filepath.Join(badDir, pkgRel)); !os.IsNotExist(err) {
		t.Errorf("corrupt file should not be placed at destination: %v", err)
	}

	// Path traversal filename in index is rejected.
	travSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/main/binary-amd64/Packages" {
			fmt.Fprintf(w, "Package: evil\nVersion: 1.0.0\nFilename: ../evil.deb\nSHA256: %s\n\n", sumHex)
			return
		}
		http.NotFound(w, r)
	}))
	defer travSrv.Close()

	if out4, err := runAptRestorePool(t, travSrv.URL, t.TempDir(), "APT_RESTORE_RETRY=0"); err == nil {
		t.Fatalf("path traversal filename: expected failure, got 0\n%s", out4)
	}
}

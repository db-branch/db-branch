package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderClientManifests_FormulaとScoopを生成する(t *testing.T) {
	dir := t.TempDir()
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	sums := strings.Join([]string{
		strings.Repeat("a", 64) + "  db-branch_1.0.0_darwin_arm64.tar.gz",
		strings.Repeat("b", 64) + "  db-branch_1.0.0_darwin_amd64.tar.gz",
		strings.Repeat("c", 64) + "  db-branch_1.0.0_linux_arm64.tar.gz",
		strings.Repeat("d", 64) + "  db-branch_1.0.0_linux_amd64.tar.gz",
		strings.Repeat("e", 64) + "  db-branch_1.0.0_windows_amd64.zip",
		strings.Repeat("f", 64) + "  db-branch_1.0.0_windows_arm64.zip",
	}, "\n") + "\n"
	if err := os.WriteFile(sumsPath, []byte(sums), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	cmd := exec.Command("bash", filepath.Join("render-client-manifests.sh"), "1.0.0", sumsPath, outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render-client-manifests.sh failed: %v\n%s", err, out)
	}

	rb, err := os.ReadFile(filepath.Join(outDir, "Formula", "db-branch.rb"))
	if err != nil {
		t.Fatal(err)
	}
	rbStr := string(rb)
	for _, want := range []string{`version "1.0.0"`, strings.Repeat("a", 64), strings.Repeat("d", 64), "db-branch_1.0.0_darwin_arm64.tar.gz"} {
		if !strings.Contains(rbStr, want) {
			t.Errorf("Formula missing %q:\n%s", want, rbStr)
		}
	}
	if strings.Contains(rbStr, "__") {
		t.Errorf("Formula still contains placeholder:\n%s", rbStr)
	}

	scoopRaw, err := os.ReadFile(filepath.Join(outDir, "bucket", "db-branch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var scoop struct {
		Version      string `json:"version"`
		Architecture map[string]struct {
			URL  string `json:"url"`
			Hash string `json:"hash"`
		} `json:"architecture"`
	}
	if err := json.Unmarshal(scoopRaw, &scoop); err != nil {
		t.Fatalf("invalid scoop JSON: %v\n%s", err, scoopRaw)
	}
	if scoop.Version != "1.0.0" || scoop.Architecture["64bit"].Hash != strings.Repeat("e", 64) || scoop.Architecture["arm64"].Hash != strings.Repeat("f", 64) {
		t.Errorf("unexpected scoop manifest: %+v", scoop)
	}

	// Missing checksum must fail.
	badSums := filepath.Join(dir, "BADSUMS")
	_ = os.WriteFile(badSums, []byte(strings.Repeat("a", 64)+"  db-branch_1.0.0_darwin_arm64.tar.gz\n"), 0o600)
	if err := exec.Command("bash", filepath.Join("render-client-manifests.sh"), "1.0.0", badSums, outDir).Run(); err == nil {
		t.Error("incomplete SHA256SUMS should fail")
	}

	// Empty token skips cleanly with 0 exit, and local bare repo push creates main branch.
	if err := exec.Command("bash", filepath.Join("push-manifest-repo.sh"), "https://example.invalid/repo.git", filepath.Join(outDir, "Formula", "db-branch.rb"), "Formula/db-branch.rb", "1.0.0", "UNSET_TOKEN_ENV_XYZ").Run(); err != nil {
		t.Errorf("unset token should skip cleanly, got %v", err)
	}
	bare := filepath.Join(dir, "bare.git")
	if out, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	cmdPush := exec.Command("bash", filepath.Join("push-manifest-repo.sh"), bare, filepath.Join(outDir, "Formula", "db-branch.rb"), "Formula/db-branch.rb", "1.0.0", "TEST_PUSH_TOK")
	cmdPush.Env = append(os.Environ(), "TEST_PUSH_TOK=dummy")
	if out, err := cmdPush.CombinedOutput(); err != nil {
		t.Fatalf("push-manifest-repo.sh failed: %v\n%s", err, out)
	}
	showOut, err := exec.Command("git", "-C", bare, "show", "main:Formula/db-branch.rb").CombinedOutput()
	if err != nil || !strings.Contains(string(showOut), `version "1.0.0"`) {
		t.Errorf("bare main:Formula/db-branch.rb = (%v, %s)", err, showOut)
	}
}

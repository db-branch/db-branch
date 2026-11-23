package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWorkflows_全Actionが40桁SHAで固定されている(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	shaPin := regexp.MustCompile(`^[^\s@]+@[0-9a-f]{40}(\s+#.*)?$`)
	usesLine := regexp.MustCompile(`^\s*-?\s*uses:\s*(.+?)\s*$`)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			m := usesLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := strings.Trim(m[1], `"'`)
			if !shaPin.MatchString(ref) {
				t.Errorf("%s:%d uses %q is not pinned to a 40-char commit SHA", filepath.Base(f), i+1, ref)
			}
		}
	}
}

func TestCI_raceテストを実行する(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "go test -race ./...") {
		t.Errorf("ci.yml does not run go test -race ./...")
	}
}

func TestMakefile_integrationにappを含みdebOssでTAGSを空にする(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	var integrationCmd string
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "integration:") && i+1 < len(lines) {
			integrationCmd = lines[i+1]
			break
		}
	}
	if !strings.Contains(integrationCmd, "./internal/app/") {
		t.Errorf("Makefile integration target = %q, want ./internal/app/", integrationCmd)
	}
	if !strings.Contains(s, "deb-oss: override TAGS=") {
		t.Errorf("Makefile deb-oss target must clear TAGS= with override")
	}
}

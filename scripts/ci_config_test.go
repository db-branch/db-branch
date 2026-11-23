package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type actionsLock struct {
	Workflows    map[string][]string `yaml:"workflows"`
	Dependencies map[string]struct {
		Ref    string `yaml:"ref"`
		Commit string `yaml:"commit"`
	} `yaml:"dependencies"`
}

func TestWorkflows_全ActionがactionsLockでSHA固定されている(t *testing.T) {
	lockRaw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "actions.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var lock actionsLock
	if err := yaml.Unmarshal(lockRaw, &lock); err != nil {
		t.Fatalf("parse actions.lock: %v", err)
	}

	files, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	shaCommit := regexp.MustCompile(`^sha1-[0-9a-f]{40}$`)
	usesLine := regexp.MustCompile(`^\s*-?\s*uses:\s*(.+?)\s*$`)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		wfKey := ".github/workflows/" + filepath.Base(f)
		lockedRefs, ok := lock.Workflows[wfKey]
		if !ok {
			t.Errorf("%s is missing from actions.lock workflows", wfKey)
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			m := usesLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := strings.Trim(m[1], `"'`)
			if idx := strings.Index(ref, " #"); idx >= 0 {
				ref = strings.TrimSpace(ref[:idx])
			}
			if !slices.Contains(lockedRefs, ref) {
				t.Errorf("%s:%d uses %q not listed in actions.lock workflows[%q]", filepath.Base(f), i+1, ref, wfKey)
			}
			dep, ok := lock.Dependencies[ref]
			if !ok {
				t.Errorf("%s:%d uses %q missing from actions.lock dependencies", filepath.Base(f), i+1, ref)
				continue
			}
			if !shaCommit.MatchString(dep.Commit) {
				t.Errorf("actions.lock dependencies[%q].commit = %q, want sha1-<40 hex>", ref, dep.Commit)
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

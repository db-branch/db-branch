package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoFilesForDocCheck(t *testing.T) []string {
	t.Helper()
	files := []string{
		filepath.Join("..", "README.md"),
		filepath.Join("..", "SECURITY.md"),
		filepath.Join("..", "SUPPORT.md"),
		filepath.Join("..", "CHANGELOG.md"),
		filepath.Join("..", "CONTRIBUTING.md"),
		filepath.Join("..", "CODE_OF_CONDUCT.md"),
	}
	docs, err := filepath.Glob(filepath.Join("..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, docs...)
	return files
}

func TestDocs_相対リンク切れが存在しない(t *testing.T) {
	mdLink := regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)
	lineAnchor := regexp.MustCompile(`^L(\d+)(?:-L(\d+))?$`)

	for _, f := range repoFilesForDocCheck(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(f)
		inFence := false
		for lineNo, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
				target := strings.TrimSpace(m[1])
				if target == "" || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
					strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
					continue
				}
				pathPart, anchor, _ := strings.Cut(target, "#")
				resolved := filepath.Join(dir, pathPart)
				info, err := os.Stat(resolved)
				if err != nil {
					t.Errorf("%s:%d broken relative link %q (%v)", filepath.ToSlash(f), lineNo+1, target, err)
					continue
				}
				if anchor != "" && !info.IsDir() {
					if lm := lineAnchor.FindStringSubmatch(anchor); lm != nil {
						targetRaw, err := os.ReadFile(resolved)
						if err != nil {
							t.Errorf("%s:%d cannot read link target %q: %v", filepath.ToSlash(f), lineNo+1, target, err)
							continue
						}
						lines := len(strings.Split(string(targetRaw), "\n"))
						for _, g := range lm[1:] {
							if g == "" {
								continue
							}
							var n int
							for _, ch := range g {
								n = n*10 + int(ch-'0')
							}
							if n > lines {
								t.Errorf("%s:%d line anchor %q exceeds %s line count (%d)", filepath.ToSlash(f), lineNo+1, target, pathPart, lines)
							}
						}
					}
				}
			}
		}
	}
}

func TestDocs_README索引がdocs配下の全ファイルを網羅する(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "docs", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(readme)
	entries, err := os.ReadDir(filepath.Join("..", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		if !strings.Contains(content, "("+e.Name()+")") {
			t.Errorf("docs/README.md does not index docs/%s", e.Name())
		}
	}
}

func TestDocs_SECURITYとHTTPAPIコメントがdbBranchAdminを正しく記載する(t *testing.T) {
	sec, err := os.ReadFile(filepath.Join("..", "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	secText := string(sec)
	if !strings.Contains(secText, "`db-branch-admin`") {
		t.Errorf("SECURITY.md must name `db-branch-admin` as the trusted group")
	}
	oldOutOfScope := regexp.MustCompile("(?i)out of scope:[^.]*`?db-branch`?\\s+group")
	if oldOutOfScope.MatchString(secText) {
		t.Errorf("SECURITY.md still lists `db-branch` group as out of scope / trusted")
	}
	if strings.Contains(strings.ToLower(secText), "in beta") {
		t.Errorf("SECURITY.md still says db-branch is in Beta")
	}

	for _, rel := range []string{
		filepath.Join("..", "internal", "interface", "httpapi", "access.go"),
		filepath.Join("..", "internal", "interface", "httpapi", "misc.go"),
	} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "in the db-branch group") || strings.Contains(string(raw), "// db-branch group") {
			t.Errorf("%s still references old `db-branch group` instead of `db-branch-admin group`", rel)
		}
	}
}

func TestDocs_OpenAPIバージョン1_0_0とGA表記が整合する(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Info struct {
			Version string `yaml:"version"`
		} `yaml:"info"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != "1.0.0" {
		t.Errorf("api/openapi.yaml info.version = %q, want 1.0.0", spec.Info.Version)
	}

	checks := []struct {
		path string
		bad  []string
	}{
		{filepath.Join("..", "README.md"), []string{"Status: Beta", "現在は Beta"}},
		{filepath.Join("..", "SUPPORT.md"), []string{"Beta project"}},
		{filepath.Join("..", "CHANGELOG.md"), []string{"Until then (Beta)"}},
		{filepath.Join("..", "docs", "design.md"), []string{
			"ステータス: Draft", "ed25519 で署名", "full_page_writes=off", "--from-dump", "`[zfs] arc_max`",
			`\n- `, // Guard against literal two-character `\n` left inside Markdown prose.
			"Debian 12",
		}},
		{filepath.Join("..", "docs", "api.md"), []string{"ステータス: Draft", "GET /v1/healthz", "/v1/runtimes/import", "`GET` | `/v1/cluster/nodes`", "`If-Match`", "`Idempotency-Key`", "`X-DB-Branch-Index`"}},
		{filepath.Join("..", "docs", "cluster-plan.md"), []string{"Suggests: proxysql"}},
		{filepath.Join("..", "docs", "roadmap.md"), []string{"v0.3.1 (Beta)"}},
		{filepath.Join("..", "docs", "runtime-modules-standard.md"), []string{"docs/security-review.md"}},
		{filepath.Join("..", "docs", "checkpoints.md"), []string{"復元の前の自動チェックポイントだけは、サーバーを止めてから取る", "checkpointing"}},
	}
	for _, c := range checks {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		for _, bad := range c.bad {
			if strings.Contains(s, bad) {
				t.Errorf("%s still contains stale text %q", c.path, bad)
			}
		}
	}
}

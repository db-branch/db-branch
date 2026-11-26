package scripts_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPublishApt_Packagesの後にReleaseとReleaseGpgとInReleaseの順で並べる(t *testing.T) {
	py := `
import importlib.util, pathlib
spec = importlib.util.spec_from_file_location("publish_apt", pathlib.Path("publish-apt.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
files = [
    "dists/stable/InRelease",
    "dists/stable/Release",
    "dists/stable/Release.gpg",
    "dists/stable/main/binary-amd64/Packages.gz",
    "dists/stable/main/binary-arm64/Packages",
    "db-branch.asc",
]
print("\n".join(mod.sort_rest(files)))
`
	out, err := exec.Command("python3", "-B", "-c", py).CombinedOutput()
	if err != nil {
		t.Fatalf("python3 failed: %v\n%s", err, out)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{
		"db-branch.asc",
		"dists/stable/main/binary-amd64/Packages.gz",
		"dists/stable/main/binary-arm64/Packages",
		"dists/stable/Release",
		"dists/stable/Release.gpg",
		"dists/stable/InRelease",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sort_rest = %v, want %v", got, want)
	}
}

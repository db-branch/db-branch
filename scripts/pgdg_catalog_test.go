package scripts_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPGDGCatalog_Debianバージョン比較でプレリリースとエポックを正しく扱う(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	scriptDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	py := `
import importlib.util, sys

spec = importlib.util.spec_from_file_location("pgdg_catalog", sys.argv[1])
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

# 1. DEPS-AUDIT repro: 18~rc1 must sort BEFORE 18.0 (never preferred as newest).
assert mod.vkey("18~rc1-1.pgdg24.04+1") < mod.vkey("18.0-1.pgdg24.04+1"), "tilde prerelease ranked above GA"
assert mod.vkey("18.0~rc1-1.pgdg24.04+1") < mod.vkey("18.0-1.pgdg24.04+1"), "minor tilde prerelease ranked above GA"
assert mod.vkey("18.7-1~pgdg24.04+1") < mod.vkey("18.7-1.pgdg24.04+1"), "revision tilde ranked above GA"

pkgs = {"postgresql-18": [
    {"Version": "18.0-1.pgdg24.04+1"},
    {"Version": "18~rc1-1.pgdg24.04+1"},
    {"Version": "18.0~rc2-1.pgdg24.04+1"},
]}
assert mod.newest(pkgs, "postgresql-18", "18")["Version"] == "18.0-1.pgdg24.04+1"
assert mod.newest(pkgs, "postgresql-18")["Version"] == "18.0-1.pgdg24.04+1"

# 2. Epoch comparison & major extraction.
assert mod.vkey("1:17.2-1.pgdg24.04+1") > mod.vkey("18.0-1.pgdg24.04+1"), "epoch 1 must beat epoch 0"
epkgs = {"postgresql-17": [
    {"Version": "17.12-1.pgdg24.04+1"},
    {"Version": "1:17.2-1.pgdg24.04+1"},
]}
assert mod.newest(epkgs, "postgresql-17", "17")["Version"] == "1:17.2-1.pgdg24.04+1"

# 3. Debian character ordering: ~ < end < letters < punctuation (+, .)
assert mod.vkey("19.0~beta1-1") < mod.vkey("19.0-1")
assert mod.vkey("19.0-1") < mod.vkey("19.0-2") < mod.vkey("19.0-10")
assert mod.vkey("19.0a-1") < mod.vkey("19.0+deb1-1")
assert mod.vkey("19.0-1") < mod.vkey("19.0-1.pgdg24.04+1")

# 4. Prerelease-only major must not be picked as a GA runtime, and major=None (libpq5) must skip upstream prereleases.
only_pre = {"postgresql-19": [{"Version": "19~beta1-1.pgdg24.04+1"}, {"Version": "19.0~rc1-1.pgdg24.04+1"}]}
try:
    mod.newest(only_pre, "postgresql-19", "19")
except SystemExit:
    pass
else:
    raise AssertionError("prerelease-only major should fail")

libpq_pkgs = {"libpq5": [
    {"Version": "18.3-1.pgdg26.04+1"},
    {"Version": "19~beta1-1.pgdg26.04+1"},
]}
assert mod.newest(libpq_pkgs, "libpq5")["Version"] == "18.3-1.pgdg26.04+1"

# 5. Leading zeros, empty revision vs -0, multiple hyphens, invalid epochs.
assert mod.deb_cmp("18.01-1", "18.1-1") == 0
assert mod.deb_cmp("18.0", "18.0-0") == 0
assert mod.deb_cmp("18.0-1", "18.0-1-2") < 0
for bad_epoch in ["-1:18.0-1", "+1:18.0-1", ":18.0-1", "abc:18.0-1"]:
    try:
        mod.deb_cmp(bad_epoch, "18.0-1")
    except ValueError:
        pass
    else:
        raise AssertionError(f"invalid epoch accepted: {bad_epoch}")

# 6. Cross-check against dpkg --compare-versions when dpkg is installed (Linux/CI).
import shutil, subprocess
if shutil.which("dpkg"):
    samples = [
        "18~rc1-1.pgdg24.04+1", "18.0~rc1-1.pgdg24.04+1", "18.0", "18.0-0", "18.0-1.pgdg24.04+1",
        "18.01-1", "18.1-1", "18.0-1", "18.0-1-2",
        "18.7-1~bpo12+1", "18.7-1", "18.7-1.pgdg24.04+5", "18.7-1.pgdg26.04+5",
        "19.0~beta1-1", "19.0-1", "19.0-2.pgdg24.04+1", "19.0-10",
        "19.0a-1", "19.0+deb1-1", "1:17.2-1.pgdg24.04+1", "1:17.2-1",
    ]
    for a in samples:
        for b in samples:
            got = mod.deb_cmp(a, b)
            op = "lt" if got < 0 else "gt" if got > 0 else "eq"
            rc = subprocess.run(["dpkg", "--compare-versions", a, op, b]).returncode
            assert rc == 0, f"mismatch for {a} vs {b}: deb_cmp={got} ({op})"
`
	cmd := exec.Command("python3", "-B", "-c", py, filepath.Join(scriptDir, "pgdg-catalog.py"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3 check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

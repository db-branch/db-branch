// Command db-branch-anon is the anon masker module (free): masked copies of
// PostgreSQL bases with PostgreSQL Anonymizer static masking
// (db-branch base mask <target> --from <base> --masker anon).
package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/db-branch/db-branch/internal/infrastructure/anon"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

var version = "dev"

func main() {
	m := &anon.Module{Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		return cmd.CombinedOutput()
	}}
	plugin.Serve(ext.Module{Name: "anon", Version: version, Masker: m, Configurable: m, Settings: m.Settings()})
}

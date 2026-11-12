// Command db-branch-greenmask is the greenmask masker module (free): it
// writes masked copies of PostgreSQL bases with Greenmask
// (db-branch base mask <target> --from <base> --masker greenmask).
package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/db-branch/db-branch/internal/infrastructure/greenmask"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

var version = "dev"

func main() {
	m := &greenmask.Module{Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		return cmd.CombinedOutput()
	}}
	plugin.Serve(ext.Module{Name: "greenmask", Version: version, Masker: m, Configurable: m, Settings: m.Settings()})
}

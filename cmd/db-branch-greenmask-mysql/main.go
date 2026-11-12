// Command db-branch-greenmask-mysql is the greenmask-mysql masker module
// (free): masked copies of MySQL bases with Greenmask v1 (beta)
// (db-branch base mask <target> --from <base> --masker greenmask-mysql).
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
	m := &greenmask.MySQL{Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		return cmd.CombinedOutput()
	}}
	plugin.Serve(ext.Module{Name: "greenmask-mysql", Version: version, Masker: m, Configurable: m, Settings: m.Settings()})
}

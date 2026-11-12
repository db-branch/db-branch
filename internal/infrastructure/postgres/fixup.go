package postgres

import (
	"fmt"
	"os"
	"path/filepath"
)

// Fixup links bin/ and lib/ in an unpacked PGDG runtime to the versioned
// binaries (usr/lib/postgresql/<major>/bin) and the bundled libpq
// (usr/lib/<triplet>), so the server finds both at fixed places.
func Fixup(dir string) error {
	for link, pattern := range map[string]string{
		"bin": "usr/lib/postgresql/*/bin",
		"lib": "usr/lib/*-linux-gnu",
	} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		if len(matches) != 1 {
			return fmt.Errorf("postgres runtime: want one %s, found %d", pattern, len(matches))
		}
		rel, _ := filepath.Rel(dir, matches[0])
		if err := os.Symlink(rel, filepath.Join(dir, link)); err != nil {
			return err
		}
	}
	return nil
}

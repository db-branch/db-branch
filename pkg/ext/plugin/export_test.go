package plugin

import (
	"io/fs"
	"log/slog"
)

// LoadForTest runs Load with a custom trust root and file ownership check so
// tests can run as non-root inside temporary directories without mutating
// package-level state.
func LoadForTest(dir, trustRoot string, ownerOf func(fs.FileInfo) (uint32, bool), log *slog.Logger) ([]*Loaded, error) {
	return (loader{trustRoot: trustRoot, ownerOf: ownerOf}).load(dir, log)
}

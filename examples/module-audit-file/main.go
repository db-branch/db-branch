// Command module-audit-file is an example db-branch module: it appends every
// audit event to a JSON Lines file. Install the binary as
// /usr/lib/db-branch/modules/audit-file (root-owned, not group/world
// writable) and restart the agent.
//
// The file is /var/log/db-branch/audit.jsonl, or $DB_BRANCH_AUDIT_FILE.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

type fileSink struct {
	mu   sync.Mutex
	path string
}

func (f *fileSink) Record(_ context.Context, e ext.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o750); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer fh.Close()
	b, _ := json.Marshal(e)
	_, err = fh.Write(append(b, '\n'))
	return err
}

func main() {
	path := os.Getenv("DB_BRANCH_AUDIT_FILE")
	if path == "" {
		path = "/var/log/db-branch/audit.jsonl"
	}
	plugin.Serve(ext.Module{Name: "audit-file", Version: "0.1.0", AuditSink: &fileSink{path: path}})
}

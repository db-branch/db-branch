package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Backups is the backup use case of this node.
type Backups interface {
	Take(ctx context.Context) (domain.BackupFile, error)
	List() ([]domain.BackupFile, error)
	Open(name string) (io.ReadCloser, error)
	Restore(ctx context.Context, r io.Reader) (domain.RestoreSummary, error)
}

func (s *Server) withBackups(w http.ResponseWriter, r *http.Request) bool {
	if s.forward(w, r, r.URL.Query().Get("node")) {
		return false
	}
	if s.Backups == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "backups are not available on this node")
		return false
	}
	return true
}

// listBackups lists this node's (or ?node=) backups.
func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	if !s.withBackups(w, r) {
		return
	}
	list, err := s.Backups.List()
	if err != nil {
		fail(w, err)
		return
	}
	if list == nil {
		list = []domain.BackupFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": s.Node, "items": list})
}

// takeBackup saves the cluster state on this node (or ?node=) now.
func (s *Server) takeBackup(w http.ResponseWriter, r *http.Request) {
	if !s.withBackups(w, r) {
		return
	}
	f, err := s.Backups.Take(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// downloadBackup sends one backup file; it holds secrets, so only admins
// may (the authorizer decides).
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// The file name says which node took it: go there unless ?node= says.
	if r.URL.Query().Get("node") == "" {
		if node := backupNode(name); node != "" && s.forward(w, r, node) {
			return
		}
	}
	if !s.withBackups(w, r) {
		return
	}
	f, err := s.Backups.Open(name)
	if err != nil {
		fail(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = io.Copy(w, f)
}

// maxRestoreBytes caps the (compressed) backup a restore reads.
const maxRestoreBytes = 64 << 20

// restoreBackup applies a backup (the request body) to the cluster. Only
// on a node's socket: restoring replaces the admin password and tokens.
func (s *Server) restoreBackup(local bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowSocketOnly(w, r, local, "run `db-branch backup restore <file>` on a node") {
			return
		}
		if s.Backups == nil {
			writeError(w, http.StatusNotImplemented, "not_implemented", "backups are not available on this node")
			return
		}
		sum, err := s.Backups.Restore(r.Context(), http.MaxBytesReader(w, r.Body, maxRestoreBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) || errors.Is(err, domain.ErrTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
				return
			}
			writeError(w, http.StatusUnprocessableEntity, "restore_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sum)
	}
}

// backupNode is the node in a backup's name (db-branch-<node>-<time>.json.gz).
func backupNode(name string) string {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(name, ".json.gz"), "db-branch-")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

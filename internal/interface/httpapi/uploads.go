package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

var errNoUploads = errors.New("uploads are not available on this node")

// upload streams a dump (the raw body, ?name=x.sql.gz) to this node's disk,
// or to ?node=, the node that will use it. It is refused before reading
// anything when its size is unknown, over the limit, or larger than the
// space left on the upload disk or the ZFS pool.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if node := r.URL.Query().Get("node"); s.forward(w, r, node) {
		return
	}
	if s.Uploads == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", errNoUploads.Error())
		return
	}
	if r.ContentLength <= 0 {
		writeError(w, http.StatusLengthRequired, "length_required", "send the file with a Content-Length")
		return
	}
	if s.PoolFree != nil {
		if free := s.PoolFree(); uint64(r.ContentLength) > free {
			writeError(w, http.StatusInsufficientStorage, "no_space",
				fmt.Sprintf("the dump is %d bytes but the ZFS pool on %s has %d free; loading needs at least as much", r.ContentLength, s.Node, free))
			return
		}
	}
	u, err := s.Uploads.Save(r.Body, r.URL.Query().Get("name"), r.ContentLength)
	switch {
	case errors.Is(err, domain.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, domain.ErrNoSpace):
		writeError(w, http.StatusInsufficientStorage, "no_space", err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"id": domain.UploadRef + u.ID, "name": u.Name, "size": u.Size, "node": s.Node})
	}
}

// uploadRoom tells a client, before it sends a dump, the largest one this
// node (or ?node=) accepts now and what limits it: "limit" (the setting),
// "disk" (the upload directory) or "pool" (the ZFS pool it loads into).
func (s *Server) uploadRoom(w http.ResponseWriter, r *http.Request) {
	if node := r.URL.Query().Get("node"); s.forward(w, r, node) {
		return
	}
	if s.Uploads == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", errNoUploads.Error())
		return
	}
	room, err := s.Uploads.Room()
	if err != nil {
		fail(w, err)
		return
	}
	reason := "limit"
	if s.PoolFree != nil {
		if pool := s.PoolFree(); pool < uint64(room) {
			room, reason = int64(pool), "pool"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": s.Node, "max_bytes": room, "reason": reason})
}

// resolveDump turns "upload:<id>" into the uploaded file's path on this
// node; other values are paths already. done removes a used upload.
func (s *Server) resolveDump(dump string) (path string, done func(), err error) {
	id, ok := strings.CutPrefix(dump, domain.UploadRef)
	if !ok {
		return dump, func() {}, nil
	}
	if s.Uploads == nil {
		return "", nil, errNoUploads
	}
	u, err := s.Uploads.Get(id)
	if err != nil {
		return "", nil, fmt.Errorf("%w (uploads stay on the node they were sent to; send it with ?node=)", err)
	}
	return u.Path, func() { _ = s.Uploads.Remove(id) }, nil
}

func (s *Server) failDump(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoUploads) {
		writeError(w, http.StatusNotImplemented, "not_implemented", err.Error())
		return
	}
	fail(w, err)
}

// loadBase loads a dump into the base's server, then (snapshot: true) takes
// a snapshot so new branches see it.
func (s *Server) loadBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	var in struct {
		Dump     string `json:"dump"`
		Snapshot bool   `json:"snapshot"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Dump == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "dump is empty: an upload id (upload:...) or a path on the base's node")
		return
	}
	path, done, err := s.resolveDump(in.Dump)
	if err != nil {
		s.failDump(w, err)
		return
	}
	s.accepted(w, r, "base.load", operation.Target{Resource: "base", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			defer done()
			err := svc.LoadBase(ctx, n, path, report)
			if err == nil && in.Snapshot {
				_, err = svc.SnapshotBase(ctx, n, report)
			}
			s.publishBase(ctx, svc, n)
			return err
		})
}

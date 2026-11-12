package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
)

type checkpointJSON struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	// Size is the space only this checkpoint holds.
	Size uint64 `json:"size"`
	// Line is the line of history it was taken on (0: the branch's first).
	Line int `json:"line"`
	// Auto marks checkpoints the system took before a restore.
	Auto bool `json:"auto"`
	// Current marks the checkpoint the branch's data descends from.
	Current bool `json:"current"`
}

func checkpointView(c domain.Checkpoint) checkpointJSON {
	return checkpointJSON{Name: c.Name, CreatedAt: ts(c.CreatedAt), Size: c.Size, Line: c.Line, Auto: c.Auto, Current: c.Current}
}

// checkpointBranch finds the branch and forwards the request to the node
// holding it. ok is false when the request is answered already.
func (s *Server) checkpointBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) (domain.Branch, bool) {
	b, err := svc.GetBranch(r.Context(), n)
	if err != nil {
		fail(w, err)
		return b, false
	}
	return b, !s.forward(w, r, b.Node)
}

// lineJSON is a line of history: the lanes the console draws.
type lineJSON struct {
	Line int  `json:"line"`
	Live bool `json:"live"`
	// Parent is the line the checkpoint this line was restored from belongs
	// to (null for the first line).
	Parent *int `json:"parent"`
	// From is that checkpoint ("reset": the branch's creation state; "": none).
	From string `json:"from"`
}

// listCheckpoints answers the checkpoints and, beside them, the structure of
// the lines (live: the line the branch runs on, head: the checkpoint its
// data descends from) so a client can draw the history.
func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if _, ok := s.checkpointBranch(w, r, svc, n); !ok {
		return
	}
	g, err := svc.CheckpointGraph(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]checkpointJSON, 0, len(g.Checkpoints))
	for _, c := range g.Checkpoints {
		out = append(out, checkpointView(c))
	}
	lines := make([]lineJSON, 0, len(g.Lines))
	for _, l := range g.Lines {
		lines = append(lines, lineJSON{Line: l.Line, Live: l.Live, Parent: l.Parent, From: l.From})
	}
	resp := items(out)
	resp["lines"], resp["live"], resp["head"] = lines, g.Live, optString(g.Head)
	writeJSON(w, http.StatusOK, resp)
}

// createCheckpoint takes a checkpoint at once (a snapshot costs nothing) and
// answers 201 with it. The body is optional: {"name": "..."}.
func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// Forwarding re-reads the body: hand it back.
	body, _ := json.Marshal(in)
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	if _, ok := s.checkpointBranch(w, r, svc, n); !ok {
		return
	}
	c, err := svc.CreateCheckpoint(r.Context(), n, in.Name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, checkpointView(c))
}

// restoreCheckpoint sets the branch back to a checkpoint in the background.
func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	cp, err := domain.ParseCheckpointName(r.PathValue("checkpoint"))
	if err != nil {
		fail(w, err)
		return
	}
	b, ok := s.checkpointBranch(w, r, svc, n)
	if !ok {
		return
	}
	if err := b.CanCheckpoint(); err != nil {
		fail(w, err)
		return
	}
	list, err := svc.ListCheckpoints(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	if !hasCheckpoint(list, cp) {
		fail(w, fmt.Errorf("checkpoint %s of branch %s: %w", cp, n, domain.ErrNotFound))
		return
	}
	s.branchAction(w, r, svc, n, "branch.restore", domain.Branch.CanCheckpoint,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			_, err := svc.RestoreCheckpoint(ctx, n, cp, report)
			return err
		})
}

func hasCheckpoint(list []domain.Checkpoint, name string) bool {
	for _, c := range list {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if _, ok := s.checkpointBranch(w, r, svc, n); !ok {
		return
	}
	if err := svc.DeleteCheckpoint(r.Context(), n, r.PathValue("checkpoint")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

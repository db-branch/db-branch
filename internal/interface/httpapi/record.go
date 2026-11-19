package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// serverBranch describes a branch to the modules that add server options.
func serverBranch(b domain.Branch, maintenance bool) ext.ServerBranch {
	return ext.ServerBranch{Name: b.Name.String(), Owner: b.Owner, Engine: string(b.Runtime.Engine), Version: b.Runtime.Version,
		Record: b.Record, Anonymous: b.Owner == Anonymous.Name, Maintenance: maintenance}
}

// ServerBranchOf is serverBranch for the code that wires the modules in.
func ServerBranchOf(b domain.Branch, maintenance bool) ext.ServerBranch {
	return serverBranch(b, maintenance)
}

// checkRecord refuses a request to record a branch that cannot be recorded:
// without a login, without a module that records, or when that module
// refuses (no licence, an engine it cannot record).
func (s *Server) checkRecord(ctx context.Context, svc Branches, p ext.Principal, in createBranchJSON) error {
	if p.ID == Anonymous.ID {
		return fmt.Errorf("%w: recording needs a login", ext.ErrForbidden)
	}
	if s.Ext.ServerOptioner == nil {
		return fmt.Errorf("%w: recording needs the history module, which is not installed (db-branch module install history)", domain.ErrInvalidName)
	}
	base, err := domain.ParseName(in.Base)
	if err != nil {
		return err
	}
	b, err := svc.GetBase(ctx, base)
	if err != nil {
		return err
	}
	_, err = s.Ext.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{Name: in.Name, Owner: in.Owner, Engine: string(b.Runtime.Engine),
		Version: b.Runtime.Version, Record: true})
	if err != nil && !errors.Is(err, ext.ErrForbidden) {
		return fmt.Errorf("%w: %v", domain.ErrInvalidName, err)
	}
	return err
}

// recordAvailable reports whether a branch can be created with recording:
// a module that records is loaded and accepts a recorded branch (it is
// licensed) of an engine that can be recorded. The console shows its checkbox
// only then.
func (s *Server) recordAvailable(ctx context.Context) bool {
	if s.Ext.ServerOptioner == nil {
		return false
	}
	for _, e := range domain.DefaultRuntimes.Engines() {
		spec, _ := domain.DefaultRuntimes.Spec(e)
		if !spec.Capabilities.HistoryRecord {
			continue
		}
		if _, err := s.Ext.ServerOptioner.ServerOptions(ctx, ext.ServerBranch{Name: "probe", Owner: "probe", Engine: string(e), Record: true}); err == nil {
			return true
		}
	}
	return false
}

package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// RefreshRequest is a base's refresh schedule as a user sets it.
type RefreshRequest struct {
	Source domain.RefreshSource
	Every  time.Duration
}

// maxRunError bounds the error text kept with a run.
const maxRunError = 2000

var errNoRefreshStore = errors.New("this cluster's store keeps no refresh schedules")

func (s *Service) refreshStore() (domain.BaseRefreshStore, error) {
	rs, ok := s.Store.(domain.BaseRefreshStore)
	if !ok {
		return nil, errNoRefreshStore
	}
	return rs, nil
}

// GetRefresh returns the base's refresh schedule (ErrNotFound when it has none).
func (s *Service) GetRefresh(ctx context.Context, name domain.Name) (domain.BaseRefresh, error) {
	rs, err := s.refreshStore()
	if err != nil {
		return domain.BaseRefresh{}, err
	}
	return rs.GetBaseRefresh(ctx, name)
}

// SetRefresh schedules the base to be reloaded from a source database. An
// empty password keeps the one already stored. Any node may do this; the
// node holding the base runs the refreshes.
func (s *Service) SetRefresh(ctx context.Context, name domain.Name, req RefreshRequest) (domain.BaseRefresh, error) {
	rs, err := s.refreshStore()
	if err != nil {
		return domain.BaseRefresh{}, err
	}
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return domain.BaseRefresh{}, err
	}
	r := domain.BaseRefresh{Base: name, Source: req.Source, Every: req.Every, UpdatedAt: s.Clock().UTC()}
	if err := r.Validate(base.Runtime.Engine); err != nil {
		return domain.BaseRefresh{}, err
	}
	if old, err := rs.GetBaseRefresh(ctx, name); err == nil && r.Source.Password == "" {
		r.Source.Password = old.Source.Password
	}
	if err := rs.SetBaseRefresh(ctx, r); err != nil {
		return domain.BaseRefresh{}, err
	}
	return rs.GetBaseRefresh(ctx, name)
}

// ClearRefresh stops refreshing the base on a schedule.
func (s *Service) ClearRefresh(ctx context.Context, name domain.Name) error {
	rs, err := s.refreshStore()
	if err != nil {
		return err
	}
	return rs.ClearBaseRefresh(ctx, name)
}

// DueRefreshes lists the bases this node holds whose refresh is due now:
// ready, not being refreshed already.
func (s *Service) DueRefreshes(ctx context.Context) ([]domain.Name, error) {
	rs, err := s.refreshStore()
	if err != nil {
		return nil, err
	}
	list, err := rs.ListBaseRefreshes(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Clock()
	var due []domain.Name
	for _, r := range list {
		base, err := s.Store.GetBase(ctx, r.Base)
		if err != nil || !s.owns(base.Node) || base.Status != domain.BaseReady || !r.Due(now) || s.refreshing(r.Base) {
			continue
		}
		due = append(due, r.Base)
	}
	return due, nil
}

func (s *Service) refreshing(n domain.Name) bool {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.running[n]
}

func (s *Service) beginRefresh(n domain.Name) bool {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.running[n] {
		return false
	}
	if s.running == nil {
		s.running = map[domain.Name]bool{}
	}
	s.running[n] = true
	return true
}

func (s *Service) endRefresh(n domain.Name) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	delete(s.running, n)
}

// RefreshBase reloads the base from its source now: dump the source with the
// runtime's client tools, load the dump (the base is in maintenance, its
// snapshots and branches untouched), delete the dump, snapshot, then prune
// old snapshots. The outcome is recorded with the schedule, success or not.
func (s *Service) RefreshBase(ctx context.Context, name domain.Name, report Reporter) error {
	rs, err := s.refreshStore()
	if err != nil {
		return err
	}
	ref, err := rs.GetBaseRefresh(ctx, name)
	if err != nil {
		return err
	}
	if s.Dumper == nil {
		return errors.New("this node cannot dump source databases")
	}
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return err
	}
	if err := s.checkOwner("base", name, base.Node); err != nil {
		return err
	}
	if err := s.checkPoolRoom(ctx); err != nil {
		return err
	}
	if !s.beginRefresh(name) {
		return fmt.Errorf("%w: base %s is already being refreshed", domain.ErrConflict, name)
	}
	defer s.endRefresh(name)

	run := domain.RefreshRun{StartedAt: s.Clock().UTC()}
	snapshot, err := s.runRefresh(ctx, base, ref, report)
	err = scrub(err, ref.Source.Password)
	run.FinishedAt, run.OK = s.Clock().UTC(), err == nil
	if err != nil {
		run.Error = truncate(err.Error(), maxRunError)
	}
	if rerr := rs.RecordBaseRefreshRun(context.WithoutCancel(ctx), name, run); rerr != nil && !errors.Is(rerr, domain.ErrNotFound) {
		err = errors.Join(err, rerr)
	}
	if err == nil {
		s.followRefresh(ctx, name, snapshot)
	}
	return err
}

func (s *Service) runRefresh(ctx context.Context, base domain.Base, ref domain.BaseRefresh, report Reporter) (string, error) {
	srv, spec, err := s.baseServer(ctx, base)
	if err != nil {
		return "", err
	}
	report.report("dumping")
	dump, err := s.Dumper.Dump(ctx, domain.SourceDumpJob{Tools: srv.Admin(spec), Source: ref.Source})
	if err != nil {
		return "", err
	}
	defer s.Dumper.Discard(dump) // also when loading fails
	err = s.LoadBase(ctx, base.Name, dump, report)
	s.Dumper.Discard(dump) // unmasked data: gone as soon as it is loaded
	if err != nil {
		return "", err
	}
	snap, err := s.SnapshotBase(ctx, base.Name, report)
	if err != nil {
		return "", err
	}
	if s.AutoPrune == nil || s.AutoPrune() {
		report.report("pruning")
		_, _ = s.PruneSnapshots(ctx) // a failure here is retried by the periodic prune
	}
	return snap.Name, nil
}

// scrub hides the source password in an error, in case a tool echoed it.
func scrub(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "***"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

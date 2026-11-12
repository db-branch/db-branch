// Package modmask uses a module's masker, mask policy and refresh follower as the agent's.
package modmask

import (
	"context"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

func access(a domain.AdminAccess) ext.DBAccess {
	return ext.DBAccess{Engine: string(a.Engine), Host: a.Host, Socket: a.Socket, Port: a.Port, User: a.User, BinDir: a.BinDir, LibDir: a.LibDir}
}

func plan(p domain.MaskPlan) ext.MaskPlan {
	return ext.MaskPlan{Source: p.Source.String(), SourceSnapshot: p.SourceSnapshot, Target: p.Target.String(), Masker: p.Masker,
		Rules: p.Rules, By: p.By, SourceDB: access(p.SourceDB), TargetDB: access(p.TargetDB)}
}

// Masker is a module's masker.
type Masker struct{ M ext.Masker }

func (m Masker) Mask(ctx context.Context, j domain.MaskJob) (string, error) {
	return m.M.Mask(ctx, ext.MaskJob{Source: access(j.Source), Target: access(j.Target), Rules: j.Rules})
}

// Policy is a module's mask policy.
type Policy struct{ P ext.MaskPolicy }

func (p Policy) PrepareMask(ctx context.Context, pl domain.MaskPlan) (string, error) {
	return p.P.PrepareMask(ctx, plan(pl))
}

func (p Policy) VerifyMask(ctx context.Context, pl domain.MaskPlan) error {
	return p.P.VerifyMask(ctx, plan(pl))
}

func (p Policy) MaskRecorded(ctx context.Context, pl domain.MaskPlan, snapshot string) error {
	return p.P.MaskRecorded(ctx, plan(pl), snapshot)
}

// Follower is a module's refresh follower.
type Follower struct{ F ext.RefreshFollower }

func (f Follower) AfterRefresh(ctx context.Context, base domain.Name, snapshot string) ([]domain.MaskRun, error) {
	runs, err := f.F.AfterRefresh(ctx, base.String(), snapshot)
	out := make([]domain.MaskRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, domain.MaskRun{Target: r.Target, Masker: r.Masker})
	}
	return out, err
}

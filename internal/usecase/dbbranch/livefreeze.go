package dbbranch

import (
	"context"
	"errors"

	"github.com/db-branch/db-branch/internal/domain"
)

// liveFreezer returns the branch's runtime provider and spec when the engine
// declares Capabilities.LiveFreeze. Without a provider (or when LiveFreeze is
// false) it returns ok=false, err=nil.
func (s *Service) liveFreezer(ctx context.Context, br domain.Branch) (domain.RuntimeProvider, domain.EngineSpec, bool, error) {
	if s.Providers == nil {
		return nil, domain.EngineSpec{}, false, nil
	}
	p, ok := s.Providers.Provider(br.Runtime.Engine)
	if !ok {
		return nil, domain.EngineSpec{}, false, nil
	}
	spec, err := p.Describe(ctx)
	if err != nil {
		return nil, domain.EngineSpec{}, false, err
	}
	return p, spec, spec.Capabilities.LiveFreeze, nil
}

// withLiveFreeze surrounds fn (taking a live checkpoint snapshot) with
// PreFreeze and PostFreeze when the branch's engine declares LiveFreeze.
// Before freezing, it writes PropFreeze="on" on dataset so Recover can unfreeze
// if the agent crashes; PostFreeze always runs best-effort, and PropFreeze is
// unset only when PostFreeze succeeds.
func (s *Service) withLiveFreeze(ctx context.Context, br domain.Branch, dataset string, fn func() error) error {
	p, spec, ok, err := s.liveFreezer(ctx, br)
	if err != nil {
		return err
	}
	if !ok {
		return fn()
	}
	if err := s.Lineage.SetProps(ctx, dataset, map[string]string{domain.PropFreeze: "on"}); err != nil {
		return err
	}
	inst := br.Instance(spec)
	tok, err := p.PreFreeze(ctx, inst)
	if err == nil {
		err = fn()
	}
	if postErr := p.PostFreeze(ctx, inst, tok); postErr != nil {
		return errors.Join(err, postErr)
	}
	return errors.Join(err, s.Lineage.Unset(ctx, dataset, domain.PropFreeze))
}

// releaseFreeze finishes a live freeze left behind by a crash during
// CreateCheckpoint: if the engine declares LiveFreeze and PropFreeze=="on" is
// still set on the live dataset, it calls PostFreeze and clears the property.
func (s *Service) releaseFreeze(ctx context.Context, br domain.Branch) error {
	if s.Lineage == nil {
		return nil
	}
	p, spec, ok, err := s.liveFreezer(ctx, br)
	if err != nil || !ok {
		return err
	}
	ds := s.branchDataset(br.Name)
	props, err := s.Lineage.Props(ctx, ds)
	if err != nil || props[domain.PropFreeze] != "on" {
		return err
	}
	if err := p.PostFreeze(ctx, br.Instance(spec), domain.FreezeToken{}); err != nil {
		return err
	}
	return s.Lineage.Unset(ctx, ds, domain.PropFreeze)
}

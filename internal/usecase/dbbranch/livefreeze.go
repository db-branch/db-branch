package dbbranch

import (
	"context"
	"errors"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	defaultFreezeTimeout     = 5 * time.Second
	defaultPostFreezeTimeout = 30 * time.Second
)

func (s *Service) freezeTimeout() time.Duration {
	if s.FreezeTimeout > 0 {
		return s.FreezeTimeout
	}
	return defaultFreezeTimeout
}

func (s *Service) freezeInstance(ctx context.Context, br domain.Branch, spec domain.EngineSpec, dataset string) (domain.InstanceSpec, error) {
	info, err := s.Storage.Info(ctx, dataset)
	if err != nil {
		return domain.InstanceSpec{}, err
	}
	inst := br.Instance(spec)
	inst.DataDir = info.Mountpoint
	return inst, nil
}

func (s *Service) unfreeze(ctx context.Context, p domain.RuntimeProvider, inst domain.InstanceSpec, dataset string, tok domain.FreezeToken) error {
	postBase := context.WithoutCancel(ctx)
	postCtx, cancel := context.WithTimeout(postBase, defaultPostFreezeTimeout)
	defer cancel()
	if err := p.PostFreeze(postCtx, inst, tok); err != nil {
		return err
	}
	return s.Lineage.Unset(postBase, dataset, domain.PropFreeze)
}

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
	inst, err := s.freezeInstance(ctx, br, spec, dataset)
	if err != nil {
		return err
	}
	if err := s.Lineage.SetProps(ctx, dataset, map[string]string{domain.PropFreeze: "on"}); err != nil {
		return err
	}
	preCtx, cancel := context.WithTimeout(ctx, s.freezeTimeout())
	tok, err := p.PreFreeze(preCtx, inst)
	cancel()
	if err == nil {
		err = fn()
	}
	return errors.Join(err, s.unfreeze(ctx, p, inst, dataset, tok))
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
	inst, err := s.freezeInstance(ctx, br, spec, ds)
	if err != nil {
		return err
	}
	return s.unfreeze(ctx, p, inst, ds, domain.FreezeToken{})
}

// Package conformance checks that a runtime module keeps the contract of
// docs/runtime-modules-standard.md: what it says in its signed manifest is
// what it does. It is the library behind cmd/db-branch-conformance.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Result is the outcome of one check.
type Result struct {
	Name string
	// Err is nil when the check passed.
	Err error
	// Note explains a check that was not run (the feature is the core's, not the module's).
	Note string
}

// Passed reports whether every check passed.
func Passed(rs []Result) bool {
	for _, r := range rs {
		if r.Err != nil {
			return false
		}
	}
	return true
}

// Provider is the part of domain.RuntimeProvider the checks call.
type Provider interface {
	Describe(ctx context.Context) (domain.EngineSpec, error)
	PrepareStart(ctx context.Context, s domain.InstanceSpec) (domain.LaunchPlan, error)
	Query(ctx context.Context, s domain.InstanceSpec, q domain.QueryRequest) (domain.QueryResult, error)
	PreFreeze(ctx context.Context, s domain.InstanceSpec) (domain.FreezeToken, error)
	CheckUpgradePath(ctx context.Context, from, to string) error
	DumpSource(ctx context.Context, job domain.SourceDumpJob, outPath string) error
	UpdateStats(ctx context.Context, s domain.InstanceSpec) error
	InstanceMetrics(ctx context.Context, s domain.InstanceSpec) ([]domain.MetricSample, error)
}

// Run checks the module m, reached through p. inst is the instance the
// checks use (directories the caller made for the purpose).
func Run(ctx context.Context, m domain.RuntimeManifest, p Provider, inst domain.InstanceSpec) []Result {
	rs := []Result{manifestMatchesDescribe(ctx, m, p)}
	rs = append(rs, capabilities(ctx, m, p, inst)...)
	rs = append(rs, launchPlan(ctx, m, p, inst))
	return rs
}

func manifestMatchesDescribe(ctx context.Context, m domain.RuntimeManifest, p Provider) Result {
	r := Result{Name: "manifest"}
	spec, err := p.Describe(ctx)
	if err != nil {
		r.Err = fmt.Errorf("Describe: %w", err)
		return r
	}
	r.Err = m.CheckDescribe(spec)
	return r
}

// probes call the provider method behind each optional feature. A feature the
// core carries out itself (checkpoints, branch_merge, history_record) has no
// module method to probe.
var probes = map[string]func(context.Context, Provider, domain.InstanceSpec) error{
	"query": func(ctx context.Context, p Provider, i domain.InstanceSpec) error {
		_, err := p.Query(ctx, i, domain.QueryRequest{SQL: "select 1", MaxRows: 1})
		return err
	},
	"live_freeze": func(ctx context.Context, p Provider, i domain.InstanceSpec) error {
		_, err := p.PreFreeze(ctx, i)
		return err
	},
	"upgrade": func(ctx context.Context, p Provider, _ domain.InstanceSpec) error {
		return p.CheckUpgradePath(ctx, "1.0", "2.0")
	},
	"source_dump": func(ctx context.Context, p Provider, _ domain.InstanceSpec) error {
		return p.DumpSource(ctx, domain.SourceDumpJob{}, "")
	},
	"update_stats": func(ctx context.Context, p Provider, i domain.InstanceSpec) error {
		return p.UpdateStats(ctx, i)
	},
	"instance_metrics": func(ctx context.Context, p Provider, i domain.InstanceSpec) error {
		_, err := p.InstanceMetrics(ctx, i)
		return err
	},
}

// capabilities: an operation declared false must answer ErrFeatureNotSupported,
// one declared true must not (it may fail for other reasons: the instance
// here is not running).
func capabilities(ctx context.Context, m domain.RuntimeManifest, p Provider, inst domain.InstanceSpec) []Result {
	var rs []Result
	declared := m.Spec().Capabilities.Features()
	for _, name := range domain.FeatureNames {
		r := Result{Name: "capability " + name}
		probe, ok := probes[name]
		if !ok {
			r.Note = "carried out by the core, not by the module"
			rs = append(rs, r)
			continue
		}
		err := probe(ctx, p, inst)
		unsupported := errors.Is(err, domain.ErrFeatureNotSupported)
		switch {
		case declared[name] && unsupported:
			r.Err = fmt.Errorf("%s is declared true but the module answers ErrFeatureNotSupported", name)
		case !declared[name] && !unsupported:
			r.Err = fmt.Errorf("%s is declared false: the module must return ErrFeatureNotSupported (got %v)", name, err)
		}
		rs = append(rs, r)
	}
	return rs
}

// launchPlan: what PrepareStart returns is written into a systemd unit by the
// core, so it must pass the core's own checks and start a program of the
// module (or its unpacked runtime), not any program on the host.
func launchPlan(ctx context.Context, m domain.RuntimeManifest, p Provider, inst domain.InstanceSpec) Result {
	r := Result{Name: "launch plan"}
	plan, err := p.PrepareStart(ctx, inst)
	if err != nil {
		r.Err = fmt.Errorf("PrepareStart: %w", err)
		return r
	}
	if r.Err = plan.Validate(); r.Err != nil {
		return r
	}
	// The manifest parser has checked that the binary lies directly under the engine's directory.
	allowed := []string{path.Dir(m.Binary) + "/"}
	if inst.RuntimeDir != "" {
		allowed = append(allowed, strings.TrimSuffix(inst.RuntimeDir, "/")+"/")
	}
	for _, dir := range allowed {
		if strings.HasPrefix(plan.Command, dir) {
			return r
		}
	}
	r.Err = fmt.Errorf("launch command %q is outside %s", plan.Command, strings.Join(allowed, " and "))
	return r
}

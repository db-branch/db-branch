package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/modcatalog"
	"github.com/db-branch/db-branch/internal/infrastructure/rtserver"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimestore"
)

// attachRuntimeModules verifies the runtime modules installed on this node
// (owner, permissions, digests, signature), starts each one that passes as its
// own OS user, accepts it only if its Describe() equals its signed manifest,
// and makes it an engine of this node: registered, with its provider and its
// DBServer. It returns the engines that are loaded, the warnings (a module
// loaded without a signature, shown by GET /v1/capabilities), offer, which makes
// the builds the modules list installable (call it once the runtime cache is
// final: it wraps the cache), and a function that stops the modules.
//
// A module that fails is logged and not counted as loaded: with the cluster
// recording it as wanted, this node then refuses the API until it is fixed.
func attachRuntimeModules(a *App, cfg config.Config, log *slog.Logger) (engines, warnings []string, offer, stop func()) {
	var stops []func()
	stop = func() {
		for _, s := range stops {
			s()
		}
	}
	catalogs := map[domain.Engine]modcatalog.Source{}
	offer = func() { offerModuleRuntimes(a, catalogs) }
	loader := runtimemodule.Loader{AllowUnsigned: cfg.AllowUnsignedRuntimes}
	for _, r := range loader.LoadAll() {
		if r.Err != nil {
			log.Error("runtime module not loaded", "engine", r.Engine, "err", r.Err)
			continue
		}
		m := r.Loaded.Manifest
		cmd, err := runtimegrpc.Command(m, runtimegrpc.SystemUser)
		if err != nil {
			log.Error("runtime module not loaded", "engine", r.Engine, "err", err)
			continue
		}
		p, stopModule, err := runtimegrpc.Start(cmd, m)
		if err != nil {
			log.Error("runtime module not loaded", "engine", r.Engine, "err", err)
			continue
		}
		if err := attachEngine(a, cfg, m, p); err != nil {
			stopModule()
			log.Error("runtime module not loaded", "engine", r.Engine, "err", err)
			continue
		}
		stops = append(stops, stopModule)
		catalogs[domain.Engine(m.Engine)] = p
		if r.Loaded.Warning != "" {
			log.Warn(r.Loaded.Warning, "engine", r.Engine)
			warnings = append(warnings, r.Loaded.Warning)
		}
		engines = append(engines, r.Engine)
	}
	return engines, warnings, offer, stop
}

// attachEngine registers the engine m declares and gives it a provider and a
// DBServer. Without a storage (a node not initialised) there is nothing to
// run on: the engine is then only declared.
func attachEngine(a *App, cfg config.Config, m domain.RuntimeManifest, p *runtimegrpc.Provider) error {
	spec := m.Spec()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if described, err := p.Describe(ctx); err == nil {
		spec.VersionPattern = described.VersionPattern // the pattern is not in the manifest
	}
	if err := domain.DefaultRuntimes.Register(spec); err != nil {
		return err
	}
	if st, ok := a.Runtimes.Store.(runtimestore.Store); ok && st.RefFixups != nil {
		// The module checks an unpacked runtime before it counts as installed.
		st.RefFixups[domain.Engine(m.Engine)] = func(ctx context.Context, ref domain.RuntimeRef, dir string) error {
			return p.PrepareRuntime(ctx, ref.Version, dir)
		}
	}
	svc, err := a.Branches()
	if err != nil {
		return nil
	}
	user := m.RunAsUser
	if user == "" {
		user = domain.RuntimeUserPrefix + m.Engine
	}
	engine := domain.Engine(m.Engine)
	svc.Servers[engine] = rtserver.NewSystem(spec, p, user, cfg.DataDir, cfg.Bind)
	a.providers[engine] = p
	return nil
}

// offerModuleRuntimes makes the builds the modules list installable: their
// catalogs join the core's, and their downloads go through a client that
// connects to public addresses only.
func offerModuleRuntimes(a *App, catalogs map[domain.Engine]modcatalog.Source) {
	if len(catalogs) == 0 {
		return
	}
	a.Runtimes.Catalog = &modcatalog.Catalog{
		Base: a.Runtimes.Catalog, Modules: catalogs, Resolve: artifact.SystemResolver, TTL: 10 * time.Minute, Now: time.Now,
	}
	if core, ok := a.Runtimes.Cache.(artifact.Cache); ok {
		external := core
		external.Client = artifact.NewPublicOnlyClient()
		a.Runtimes.Cache = modcatalog.Cache{
			Core: core, External: external,
			Engines: func(e domain.Engine) bool { _, ok := catalogs[e]; return ok },
		}
	}
}

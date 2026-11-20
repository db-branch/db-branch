package app

import (
	"log/slog"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
)

// checkRuntimeModules verifies the runtime modules installed on this node
// (owner, permissions, digests, signature) and returns the engines that pass.
// A module that fails is logged and not counted as loaded: with the cluster
// recording it as wanted, this node then refuses the API until it is fixed.
func checkRuntimeModules(cfg config.Config, log *slog.Logger) []string {
	loader := runtimemodule.Loader{AllowUnsigned: cfg.AllowUnsignedRuntimes}
	var engines []string
	for _, r := range loader.LoadAll() {
		if r.Err != nil {
			log.Error("runtime module not loaded", "engine", r.Engine, "err", r.Err)
			continue
		}
		if r.Loaded.Warning != "" {
			log.Warn(r.Loaded.Warning, "engine", r.Engine)
		}
		engines = append(engines, r.Engine)
	}
	return engines
}

// Command db-branch is the entry point. Wiring lives in internal/app.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/interface/cli"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	node := app.SystemNode(os.Stderr)
	code := cli.App{
		Version: version,
		Stdin:   os.Stdin, Stdout: os.Stdout,
		Stderr: os.Stderr,
		Client: func(socket string) cli.Client { return cli.NewSocketClient(socket) },
		Agent: func(ctx context.Context, cfg config.Config) error {
			return app.RunAgent(ctx, cfg, app.AgentOptions{Version: version, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))})
		},
		Init:                  node.Init,
		UseEvaluationPassword: node.UseEvaluationPassword,
		ResetPassword:         node.ResetPassword,
		ClusterInit:           node.ClusterInit,
		ClusterJoin:           node.ClusterJoin,
		ClusterAlignIDs:       node.AlignIDs,
		RuntimeModules: func(cfg config.Config, prompter rtmodule.TrustPrompter) cli.RuntimeModuleService {
			return node.RuntimeModules(cfg, prompter)
		},
	}.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

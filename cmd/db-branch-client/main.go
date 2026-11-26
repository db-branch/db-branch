// Command db-branch (client) is the standalone remote CLI for macOS, Windows, and Linux.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/db-branch/db-branch/internal/clientapp"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cliApp := cli.App{
		Version:    version,
		ClientOnly: true,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
	clientapp.Configure(&cliApp)
	code := cliApp.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

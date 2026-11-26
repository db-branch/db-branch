// Package cli implements the db-branch command line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/usecase/idalign"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

// ExitError associates a specific process exit code with a CLI error.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// App is the db-branch command line application. The hooks are supplied by
// the composition root so the CLI stays free of infrastructure.
type App struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	// Client connects to the local agent's API socket.
	Client func(socket string) Client
	// Agent runs the node agent until ctx ends.
	Agent func(ctx context.Context, cfg config.Config) error
	// Init prepares the node on pool and returns the initial admin password
	// (empty when one was already set).
	Init func(ctx context.Context, cfg config.Config, pool string) (string, error)
	// UseEvaluationPassword sets the password "admin" that has to be changed at
	// the first console sign-in (init --evaluation).
	UseEvaluationPassword func(cfg config.Config) error
	// ResetPassword sets a new random admin password and returns it.
	ResetPassword func(cfg config.Config) (string, error)
	// ClusterInit turns this node into a one-node cluster.
	ClusterInit func(ctx context.Context, cfg config.Config) error
	// ClusterJoin joins the cluster that addr belongs to.
	ClusterJoin func(ctx context.Context, cfg config.Config, addr, token string) error
	// ClusterAlignIDs moves this node's db-branch user and group to the ids
	// the cluster uses.
	ClusterAlignIDs func(ctx context.Context, cfg config.Config, ids domain.ServiceIDs) (idalign.Result, error)
	// RuntimeModules builds the local runtime module admin service for cfg.
	RuntimeModules func(cfg config.Config, prompter rtmodule.TrustPrompter) RuntimeModuleService
	// EffectiveUID returns the caller's effective UID (defaults to os.Geteuid).
	EffectiveUID func() int
	// IsTTY reports whether r is an interactive terminal (defaults to os.ModeCharDevice check).
	IsTTY func(r io.Reader) bool
	// Poll is how often operations are polled while waiting.
	Poll time.Duration

	// ClientOnly limits the CLI to remote HTTPS client commands (no node admin).
	ClientOnly bool
	// NewClientStores builds ClientConfigStore and SecretStore for clientConfigPath.
	NewClientStores func(clientConfigPath string) (ClientConfigStore, SecretStore, error)
	// RemoteClient connects to a remote db-branch server over HTTPS.
	RemoteClient func(RemoteOptions) (Client, error)
	// ProbeTLS checks the remote server's TLS 1.3 certificate for login TOFU.
	ProbeTLS func(ctx context.Context, rawURL, caCert, expectedSHA256 string) (CertSummary, error)
	// ReadSecret reads a secret line from r with terminal echo disabled when possible.
	ReadSecret func(io.Reader) (string, error)
	// Getenv looks up environment variables (defaults to os.Getenv).
	Getenv func(string) string

	configPath       string
	clientConfigFlag string
	profileFlag      string
	serverFlag       string
	caFlag           string
	output           string
}

// Run executes the command given by args and returns the process exit code.
func (a App) Run(ctx context.Context, args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(a.Stderr, "Error:", err)
		var exitErr *ExitError
		if errors.As(err, &exitErr) && exitErr.Code > 0 {
			return exitErr.Code
		}
		return 1
	}
	return 0
}

func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "db-branch",
		Short:         "Instant database branches on ZFS clones",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVarP(&a.output, "output", "o", "table", "output format: table or json")
	root.PersistentFlags().StringVar(&a.profileFlag, "profile", "", "remote profile name from client.toml")
	root.PersistentFlags().StringVar(&a.serverFlag, "server", "", "remote db-branch HTTPS server URL")
	root.PersistentFlags().StringVar(&a.caFlag, "ca", "", "CA certificate PEM file for remote HTTPS")
	if a.ClientOnly {
		root.PersistentFlags().StringVar(&a.clientConfigFlag, "config", "", "path to client.toml")
		root.AddCommand(
			a.versionCommand(), a.loginCommand(), a.logoutCommand(), a.profileCommand(),
			a.branchCommand(), a.baseCommand(),
		)
		return root
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", config.DefaultPath, "path to config.toml")
	root.PersistentFlags().StringVar(&a.clientConfigFlag, "client-config", "", "path to client.toml")
	root.AddCommand(
		a.versionCommand(), a.loginCommand(), a.logoutCommand(), a.profileCommand(),
		a.agentCommand(), a.initCommand(), a.adminCommand(),
		a.branchCommand(), a.baseCommand(), a.runtimeCommand(), a.clusterCommand(), a.routeCommand(), a.moduleCommand(), a.settingsCommand(), a.tokenCommand(), a.backupCommand(), a.updateCommand(),
	)
	return root
}

func (a *App) loadConfig() (config.Config, error) { return config.Load(a.configPath) }

// client selects either the remote HTTPS client or the local agent UNIX socket.
func (a *App) client() (Client, error) {
	if a.useRemoteTransport() {
		return a.remoteClient()
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	return a.Client(cfg.Socket), nil
}

func (a *App) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.output == "json" {
				return a.printJSON(map[string]string{"version": a.Version})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "db-branch %s\n", a.Version)
			return err
		},
	}
}

func (a *App) agentCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Run the node agent (started by systemd)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if a.Agent != nil {
				return a.Agent(cmd.Context(), cfg)
			}
			log := slog.New(slog.NewTextHandler(a.Stderr, nil))
			log.Info("agent started", "version", a.Version, "data_dir", cfg.DataDir, "pool", cfg.Pool)
			<-cmd.Context().Done()
			log.Info("agent stopped")
			return nil
		},
	}
}

func (a *App) initCommand() *cobra.Command {
	var pool, storage string
	var evaluation bool
	cmd := &cobra.Command{
		Use:   "init --pool <pool> | --storage <module>",
		Short: "Prepare this node: ZFS datasets (or a module's storage), ARC limit, admin password, TLS certificate",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case pool != "" && storage != "":
				return fmt.Errorf("give --pool (a ZFS pool on this node) or --storage (a module's storage), not both")
			case storage != "" && !moduleName.MatchString(storage):
				return fmt.Errorf("--storage %q: a module name, e.g. fsx", storage)
			case pool == "" && storage == "":
				return fmt.Errorf("--pool is required (the ZFS pool db-branch may use), or --storage <module>")
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if storage != "" {
				// The module's datasets are named under it, like a pool.
				cfg.Storage, pool = storage, storage
			}
			password, err := a.Init(cmd.Context(), cfg, pool)
			if err != nil {
				return err
			}
			if storage != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Initialized db-branch on the storage of the %s module. Install and configure it: db-branch module install %s\n", storage, storage)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Initialized db-branch on pool %s.\n", pool)
			}
			if password != "" && evaluation {
				if a.UseEvaluationPassword == nil {
					return fmt.Errorf("--evaluation is not available here")
				}
				if err := a.UseEvaluationPassword(cfg); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\nWeb console: https://<this host>%s  user: admin  password: admin\n", cfg.Console.Listen)
				fmt.Fprintln(cmd.OutOrStdout(), "The console asks you to change the password at the first sign-in. Use this only for a throwaway evaluation VM.")
			} else if password != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\nWeb console: https://<this host>%s  user: admin  password: %s\n", cfg.Console.Listen, password)
				fmt.Fprintln(cmd.OutOrStdout(), "The password is shown only once (also saved in /etc/db-branch/initial-admin-password).")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "ZFS pool to use")
	cmd.Flags().BoolVar(&evaluation, "evaluation", false, "for a throwaway evaluation VM: the first password is \"admin\" and must be changed at the first console sign-in")
	cmd.Flags().StringVar(&storage, "storage", "", "use this module's storage instead of a ZFS pool (e.g. fsx)")
	return cmd
}

// moduleName is how modules are named.
var moduleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func (a *App) adminCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administer the web console login"}
	cmd.AddCommand(&cobra.Command{
		Use:   "reset-password",
		Short: "Set a new random admin password (for the whole cluster) and end every console session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			// Through the agent first: in a cluster the password lives in the
			// replicated state. Without a running agent, a single node's
			// password file is changed directly.
			var out struct {
				Password string `json:"password"`
			}
			err = a.Client(cfg.Socket).Do(cmd.Context(), "POST", "/v1/auth/reset", nil, &out)
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				return err
			}
			password := out.Password
			if err != nil {
				if password, err = a.ResetPassword(cfg); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "New admin password: %s\n", password)
			return nil
		},
	})
	return cmd
}

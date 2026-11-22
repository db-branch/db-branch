package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type moduleView struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Wanted  bool   `json:"wanted"`
	Nodes   []struct {
		Node    string `json:"node"`
		Loaded  string `json:"loaded"`
		Current bool   `json:"current"`
	} `json:"nodes"`
}

func (a *App) moduleCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "module", Short: "Manage cluster modules and local runtime modules"}
	var (
		version   string
		trustKey  string
		upgrade   bool
		noRestart bool
	)
	install := &cobra.Command{
		Use:   "install <name|github.com/owner/repo[@tag]>",
		Short: "Install a cluster APT module or a local runtime module from GitHub Releases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if isRuntimeInstallTarget(target) {
				if version != "" {
					return exitError(exitCodeUsage, fmt.Errorf("--version is only for cluster APT modules; specify @<tag> on the GitHub reference"))
				}
				return a.runRuntimeModuleInstall(cmd.Context(), target, trustKey, upgrade, noRestart)
			}
			if trustKey != "" || upgrade || noRestart {
				return exitError(exitCodeUsage, fmt.Errorf("--trust-key, --upgrade, and --no-restart are only for local runtime modules (github.com/<owner>/<repo>)"))
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var m moduleView
			if err := c.Do(cmd.Context(), "POST", "/v1/modules", map[string]any{"name": target, "version": version}, &m); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "module %s %s: every node installs it and restarts its agent; see `db-branch module list`\n", m.Name, m.Version)
			return nil
		},
	}
	install.Flags().StringVar(&version, "version", "", "package version for cluster APT modules (default: the newest available)")
	install.Flags().StringVar(&trustKey, "trust-key", "", "expected SHA-256 fingerprint (sha256:<64hex>) for trusting an unknown runtime module signing key")
	install.Flags().BoolVar(&upgrade, "upgrade", false, "upgrade an already installed local runtime module")
	install.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart db-branch.service after installing a local runtime module")
	var (
		removeRuntime   bool
		removeNoRestart bool
	)
	remove := &cobra.Command{
		Use:   "remove <name|engine>",
		Short: "Remove a cluster APT module or a locally installed runtime module",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			isLocal := removeRuntime
			if !isLocal {
				var err error
				isLocal, err = a.hasLocalRuntimeModule(name)
				if err != nil {
					return err
				}
			}
			if isLocal {
				return a.runRuntimeModuleRemove(cmd.Context(), name, removeNoRestart)
			}
			if removeNoRestart {
				return exitError(exitCodeUsage, fmt.Errorf("--no-restart is only for local runtime modules"))
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			return c.Do(cmd.Context(), "DELETE", "/v1/modules/"+name, nil, nil)
		},
	}
	remove.Flags().BoolVar(&removeRuntime, "runtime", false, "remove a locally installed runtime module")
	remove.Flags().BoolVar(&removeNoRestart, "no-restart", false, "do not restart db-branch.service after removing a local runtime module")
	forget := &cobra.Command{
		Use:   "forget <name>",
		Short: "Forget a removed cluster module so it is no longer listed",
		Long:  "Delete the cluster record of a module whose state is removed. Only removed modules can be forgotten; to install a module again, run `db-branch module install <name>`.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := a.client()
			if err != nil {
				return err
			}
			if err := c.Do(cmd.Context(), "POST", "/v1/modules/"+url.PathEscape(name)+"/forget", nil, nil); err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
					return exitError(exitCodeConflict, err)
				}
				return err
			}
			if a.output == "json" {
				return a.printJSON(map[string]any{"name": name, "forgotten": true})
			}
			fmt.Fprintf(a.Stdout, "Forgot module %s (the cluster no longer lists it)\n", name)
			return nil
		},
	}
	var listRuntime bool
	list := &cobra.Command{
		Use: "list", Short: "List cluster modules and locally installed runtime modules", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if listRuntime {
				return a.runRuntimeModuleList()
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []moduleView `json:"items"`
			}
			apiErr := c.Do(cmd.Context(), "GET", "/v1/modules", nil, &out)
			if a.output == "json" {
				if apiErr != nil {
					return apiErr
				}
				return a.printJSON(out.Items)
			}
			if a.RuntimeModules == nil {
				if apiErr != nil {
					return apiErr
				}
				return a.printClusterModulesTable(out.Items)
			}
			var httpErr *APIError
			if errors.As(apiErr, &httpErr) {
				return apiErr
			}
			svc, err := a.rtService(nil)
			if err != nil {
				return err
			}
			if apiErr != nil {
				fmt.Fprintf(a.Stderr, "Note: agent unreachable (%v); showing local runtime modules only.\n", apiErr)
			} else {
				fmt.Fprintln(a.Stdout, "CLUSTER MODULES")
				if err := a.printClusterModulesTable(out.Items); err != nil {
					return err
				}
				fmt.Fprintln(a.Stdout)
			}
			fmt.Fprintln(a.Stdout, "RUNTIME MODULES (LOCAL NODE)")
			return a.printRuntimeModulesTable(svc.List())
		},
	}
	list.Flags().BoolVar(&listRuntime, "runtime", false, "list locally installed runtime modules only")
	cmd.AddCommand(install, remove, forget, list, a.moduleTrustCommand())
	return cmd
}

func (a *App) moduleTrustCommand() *cobra.Command {
	trust := &cobra.Command{
		Use:   "trust",
		Short: "Manage trusted Ed25519 public keys for local runtime modules",
	}
	var expectedFP string
	add := &cobra.Command{
		Use:   "add <key_id> <pub-file|->",
		Short: "Register a trusted Ed25519 public key (run as root)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.runTrustAdd(args[0], args[1], expectedFP)
		},
	}
	add.Flags().StringVar(&expectedFP, "fingerprint", "", "expected SHA-256 fingerprint (sha256:<64hex>)")
	list := &cobra.Command{
		Use:   "list",
		Short: "List trusted Ed25519 public keys",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.runTrustList()
		},
	}
	remove := &cobra.Command{
		Use:   "remove <key_id>",
		Short: "Remove a trusted Ed25519 public key (run as root)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTrustRemove(cmd.Context(), args[0])
		},
	}
	trust.AddCommand(add, list, remove)
	return trust
}

func (a *App) printClusterModulesTable(items []moduleView) error {
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODULE\tVERSION\tSTATE\tNODES")
	for _, m := range items {
		state, done := "enabled", 0
		if !m.Wanted {
			state = "removed"
		}
		var nodes []string
		for _, n := range m.Nodes {
			mark := n.Node
			if n.Current {
				done++
			} else if m.Wanted {
				mark += "(pending)"
			}
			nodes = append(nodes, mark)
		}
		if m.Wanted {
			state = fmt.Sprintf("%s %d/%d", state, done, len(m.Nodes))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, m.Version, state, strings.Join(nodes, ","))
	}
	return tw.Flush()
}

func (a *App) settingsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "settings", Short: "Show and change cluster-wide settings"}
	show := &cobra.Command{
		Use: "list", Short: "Show every setting", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []struct {
					Scope    string `json:"scope"`
					Settings []struct {
						Key     string `json:"key"`
						Value   string `json:"value"`
						Default string `json:"default"`
						// SecretSet: a secret's value is never returned, only whether one is stored.
						SecretSet *bool `json:"secret_set"`
					} `json:"settings"`
				} `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/settings", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SCOPE\tKEY\tVALUE\tDEFAULT")
			for _, sc := range out.Items {
				for _, s := range sc.Settings {
					if s.SecretSet != nil && *s.SecretSet {
						s.Value = "(set)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sc.Scope, s.Key, orDash(&s.Value), orDash(&s.Default))
				}
			}
			return tw.Flush()
		},
	}
	var clear []string
	set := &cobra.Command{
		Use: "set <scope> <key>=<value>...", Short: "Change settings (scope: core or a module; an empty value restores the default, except for a secret: remove it with --clear <key>)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 && len(clear) == 0 {
				return errors.New("give key=value or --clear <key>")
			}
			values := map[string]string{}
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q: use key=value", kv)
				}
				values[k] = v
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			body := map[string]any{"values": values}
			if len(clear) > 0 {
				body["clear"] = clear
			}
			return c.Do(cmd.Context(), "PUT", "/v1/settings/"+args[0], body, nil)
		},
	}
	set.Flags().StringSliceVar(&clear, "clear", nil, "remove a secret setting (repeatable)")
	cmd.AddCommand(show, set)
	return cmd
}

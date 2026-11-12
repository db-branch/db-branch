package cli

import (
	"fmt"
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
	cmd := &cobra.Command{Use: "module", Short: "Enable modules on every node of the cluster"}
	var version string
	install := &cobra.Command{
		Use: "install <name>", Short: "Install a module (package db-branch-<name>) on every node", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var m moduleView
			if err := c.Do(cmd.Context(), "POST", "/v1/modules", map[string]any{"name": args[0], "version": version}, &m); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "module %s %s: every node installs it and restarts its agent; see `db-branch module list`\n", m.Name, m.Version)
			return nil
		},
	}
	install.Flags().StringVar(&version, "version", "", "package version (default: the newest available)")
	remove := &cobra.Command{
		Use: "remove <name>", Short: "Remove a module from every node", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return c.Do(cmd.Context(), "DELETE", "/v1/modules/"+args[0], nil, nil)
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List modules and where they run", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []moduleView `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/modules", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "MODULE\tVERSION\tSTATE\tNODES")
			for _, m := range out.Items {
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
		},
	}
	cmd.AddCommand(install, remove, list)
	return cmd
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
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sc.Scope, s.Key, orDash(&s.Value), orDash(&s.Default))
				}
			}
			return tw.Flush()
		},
	}
	set := &cobra.Command{
		Use: "set <scope> <key>=<value>...", Short: "Change settings (scope: core or a module; an empty value restores the default)",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			return c.Do(cmd.Context(), "PUT", "/v1/settings/"+args[0], map[string]any{"values": values}, nil)
		},
	}
	cmd.AddCommand(show, set)
	return cmd
}

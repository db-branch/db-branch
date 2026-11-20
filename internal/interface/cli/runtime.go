package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type runtimeList struct {
	Items []struct {
		Runtime string `json:"runtime"`
		Engine  string `json:"engine"`
	} `json:"items"`
}

func (a *App) runtimeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "runtime", Short: "Manage database engine versions"}

	withRef := func(f func(cmd *cobra.Command, c Client, ref domain.RuntimeRef) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			ref, err := parseRuntimeArg(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			return f(cmd, c, ref)
		}
	}
	list := func(path string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out runtimeList
			if err := c.Do(cmd.Context(), "GET", path, nil, &out); err != nil {
				return err
			}
			for _, r := range out.Items {
				if len(args) == 0 || r.Engine == args[0] {
					fmt.Fprintln(a.Stdout, r.Runtime)
				}
			}
			return nil
		}
	}
	cmd.AddCommand(
		&cobra.Command{Use: "available [mysql|postgres]", Short: "List versions that can be installed",
			Args: cobra.MaximumNArgs(1), RunE: list("/v1/runtimes/available")},
		&cobra.Command{Use: "list", Short: "List installed versions", Args: cobra.NoArgs, RunE: list("/v1/runtimes")},
		&cobra.Command{Use: "install <engine@version>", Short: "Install a version (in a cluster: on every node, fetched from the leader)", Args: cobra.ExactArgs(1),
			RunE: withRef(func(cmd *cobra.Command, c Client, ref domain.RuntimeRef) error {
				if err := a.start(cmd.Context(), c, "POST", "/v1/runtimes", map[string]any{"runtime": ref.String()}, false); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "installed %s\n", ref)
				return nil
			})},
		&cobra.Command{Use: "remove <engine@version>", Short: "Remove a version (in a cluster: from every node where nothing uses it)", Args: cobra.ExactArgs(1),
			RunE: withRef(func(cmd *cobra.Command, c Client, ref domain.RuntimeRef) error {
				if err := c.Do(cmd.Context(), "DELETE", "/v1/runtimes/"+string(ref.Engine)+"/"+ref.Version, nil, nil); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "removed %s\n", ref)
				return nil
			})},
	)
	return cmd
}

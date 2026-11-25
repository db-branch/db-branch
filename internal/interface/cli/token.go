package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func (a *App) tokenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "API tokens for scripts and LLM agents (MCP)"}
	var scope, ttl, team string
	var bases []string
	create := &cobra.Command{
		Use: "create <name>", Short: "Issue a token (shown once)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Token string `json:"token"`
				Scope string `json:"scope"`
			}
			body := map[string]any{"name": args[0], "scope": scope, "ttl": ttl}
			if len(bases) > 0 {
				body["bases"] = bases
			}
			if team != "" {
				body["team"] = team
			}
			if err := c.Do(cmd.Context(), "POST", "/v1/tokens", body, &out); err != nil {
				return err
			}
			fmt.Fprintln(a.Stdout, out.Token)
			fmt.Fprintf(a.Stderr, "scope %s; shown only now. Use it as \"Authorization: Bearer <token>\" (MCP: https://<node>:7080/v1/mcp).\n", out.Scope)
			return nil
		},
	}
	create.Flags().StringVar(&scope, "scope", "branches", "branches (create and use branches, read bases/runtimes/operations/metrics) or full")
	create.Flags().StringVar(&ttl, "ttl", "", "expire after this long, e.g. 720h (default: never)")
	create.Flags().StringSliceVar(&bases, "bases", nil, "limit a branches token to these base names")
	create.Flags().StringVar(&team, "team", "", "link a branches token to this team's limits and branch sharing")
	list := &cobra.Command{
		Use: "list", Short: "List tokens", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []struct {
					ID        string   `json:"id"`
					Name      string   `json:"name"`
					Scope     string   `json:"scope"`
					Bases     []string `json:"bases"`
					Team      string   `json:"team"`
					CreatedAt string   `json:"created_at"`
					ExpiresAt *string  `json:"expires_at"`
				} `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/tokens", nil, &out); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tBASES\tTEAM\tCREATED\tEXPIRES")
			for _, t := range out.Items {
				basesCol := "-"
				if len(t.Bases) > 0 {
					basesCol = strings.Join(t.Bases, ",")
				}
				teamCol := "-"
				if t.Team != "" {
					teamCol = t.Team
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Scope, basesCol, teamCol, t.CreatedAt, orDash(t.ExpiresAt))
			}
			return tw.Flush()
		},
	}
	revoke := &cobra.Command{
		Use: "revoke <id>", Short: "Revoke a token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return c.Do(cmd.Context(), "DELETE", "/v1/tokens/"+args[0], nil, nil)
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

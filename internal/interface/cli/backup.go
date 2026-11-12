package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type backupFile struct {
	Name    string `json:"name"`
	Size    uint64 `json:"size"`
	TakenAt string `json:"taken_at"`
}

func (a *App) backupCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Back up and restore the cluster state (settings, users, tokens, module data)"}
	cmd.AddCommand(&cobra.Command{
		Use: "now", Short: "Save the cluster state on this node now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var f backupFile
			if err := c.Do(cmd.Context(), "POST", "/v1/backups", nil, &f); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "%s (%s)\n", f.Name, HumanBytes(f.Size))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List this node's backups, newest first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []backupFile `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/backups", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "FILE\tSIZE\tTAKEN")
			for _, f := range out.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Name, HumanBytes(f.Size), f.TakenAt)
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "restore <file>",
		Short: "Apply a backup to this cluster: settings, users, tokens, admin password, modules",
		Long: "Restores the configuration from a backup (e.g. after rebuilding a cluster). Bases and branches are not\n" +
			"restored: their data is not in the backup. The admin password and tokens become the backup's.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var sum struct {
				Node            string   `json:"node"`
				TakenAt         string   `json:"taken_at"`
				Settings        int      `json:"settings"`
				Tokens          int      `json:"tokens"`
				Modules         int      `json:"modules"`
				ModuleData      int      `json:"module_data"`
				RouteUsers      int      `json:"route_users"`
				Runtimes        int      `json:"runtimes"`
				Admin           bool     `json:"admin"`
				SkippedBases    []string `json:"skipped_bases"`
				SkippedBranches []string `json:"skipped_branches"`
			}
			if err := c.Do(cmd.Context(), "POST", "/v1/backups/restore", Raw(data), &sum); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "restored the state of %s taken %s: %d setting scopes, %d tokens, %d modules (%d records), %d route users, %d engines",
				sum.Node, sum.TakenAt, sum.Settings, sum.Tokens, sum.Modules, sum.ModuleData, sum.RouteUsers, sum.Runtimes)
			if sum.Admin {
				fmt.Fprint(a.Stdout, ", the admin password")
			}
			fmt.Fprintln(a.Stdout)
			if len(sum.SkippedBases)+len(sum.SkippedBranches) > 0 {
				fmt.Fprintf(a.Stderr, "not restored (their data is not in backups): bases %s; branches %s. Create the bases again from their dumps.\n",
					orNone(sum.SkippedBases), orNone(sum.SkippedBranches))
			}
			return nil
		},
	})
	return cmd
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

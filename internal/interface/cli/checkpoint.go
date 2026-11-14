package cli

import (
	"errors"
	"fmt"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type checkpointView struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Size      uint64 `json:"size"`
	Line      int    `json:"line"`
	Auto      bool   `json:"auto"`
	Current   bool   `json:"current"`
	// Runtime is what the branch ran when it was taken.
	Runtime string `json:"runtime"`
	// Warning is set when the branch uses more than half of the pool's free space.
	Warning string `json:"warning,omitempty"`
}

func checkpointPath(n domain.Name, rest ...string) string {
	p := "/v1/branches/" + n.String() + "/checkpoints"
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

// checkpointCommand saves a checkpoint; its subcommand deletes one.
func (a *App) checkpointCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "checkpoint <name> [--name <checkpoint>]",
		Short: "Save the branch's current state as a checkpoint (instant, the branch keeps running)",
		Args:  cobra.ExactArgs(1),
		RunE: checkpointsOnly(a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			if name != "" {
				if _, err := domain.ParseCheckpointName(name); err != nil {
					return fmt.Errorf("--name: %w", err)
				}
			}
			var cp checkpointView
			if err := c.Do(cmd.Context(), "POST", checkpointPath(n), map[string]any{"name": name}, &cp); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(cp)
			}
			fmt.Fprintf(a.Stdout, "Saved checkpoint %s of %s\n", cp.Name, n)
			if cp.Warning != "" {
				fmt.Fprintf(a.Stderr, "Warning: %s\n", cp.Warning)
			}
			return nil
		})),
	}
	cmd.Flags().StringVar(&name, "name", "", "checkpoint name: lowercase letters, digits and hyphens (default: the time)")
	del := &cobra.Command{
		Use:   "delete <name> <checkpoint>",
		Short: "Delete one checkpoint (the others are untouched)",
		Args:  cobra.ExactArgs(2),
		RunE: checkpointsOnly(a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			cp, err := domain.ParseCheckpointName(cmd.Flags().Arg(1))
			if err != nil {
				return err
			}
			if err := c.Do(cmd.Context(), "DELETE", checkpointPath(n, cp), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "Deleted checkpoint %s of %s\n", cp, n)
			return nil
		})),
	}
	cmd.AddCommand(del)
	return cmd
}

func (a *App) checkpointsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "checkpoints <name>",
		Short: "List a branch's checkpoints",
		Args:  cobra.ExactArgs(1),
		RunE: checkpointsOnly(a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			var out struct {
				Items []checkpointView `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", checkpointPath(n), nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			if len(out.Items) == 0 {
				fmt.Fprintf(a.Stdout, "No checkpoints. Save one with: db-branch branch checkpoint %s\n", n)
				return nil
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCREATED\tSIZE\tLINE\tRUNTIME\t")
			for _, cp := range out.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", cp.Name, localTime(cp.CreatedAt), HumanBytes(cp.Size), cp.Line, orDash(&cp.Runtime), checkpointNotes(cp))
			}
			return tw.Flush()
		})),
	}
}

func checkpointNotes(cp checkpointView) string {
	notes := ""
	if cp.Current {
		notes = "current"
	}
	if cp.Auto {
		if notes != "" {
			notes += ", "
		}
		notes += "auto"
	}
	return notes
}

// localTime shows an API timestamp the way people read it.
func localTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func (a *App) restoreCheckpoint(noWait *bool) func(*cobra.Command, []string) error {
	return checkpointsOnly(a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
		cp, err := domain.ParseCheckpointName(cmd.Flags().Arg(1))
		if err != nil {
			return err
		}
		// The restore replaces the current state and saves nothing by itself: say so,
		// and how to keep it. A note only: it asks nothing and changes no exit status.
		fmt.Fprintf(a.Stderr, "The current state of %s is replaced and is not saved. To keep it, save a checkpoint first: db-branch branch checkpoint %s\n", n, n)
		if err := a.start(cmd.Context(), c, "POST", checkpointPath(n, cp, "restore"), nil, *noWait); err != nil || *noWait {
			return err
		}
		fmt.Fprintf(a.Stdout, "Restored %s to %s.\n", n, cp)
		return nil
	}))
}

// checkpointsOnly turns the agent's "checkpoints_unavailable" answer (module
// or shared storage) into one plain line.
func checkpointsOnly(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "checkpoints_unavailable" {
			return errors.New("Checkpoints are not available on this storage: they need the node's own ZFS (not module or shared storage)")
		}
		return err
	}
}

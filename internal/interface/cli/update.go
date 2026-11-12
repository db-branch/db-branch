package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type updateView struct {
	CheckedAt   time.Time `json:"checked_at"`
	CheckedBy   string    `json:"checked_by"`
	LatestMinor string    `json:"latest_minor"`
	Nodes       []struct {
		Name    string `json:"name"`
		Leader  bool   `json:"leader"`
		Down    bool   `json:"down"`
		Version string `json:"version"`
		Update  *struct {
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"update"`
	} `json:"nodes"`
	Packages []struct {
		Name      string `json:"name"`
		Installed string `json:"installed"`
		Mixed     bool   `json:"mixed"`
		Available []struct {
			Version  string `json:"version"`
			Kind     string `json:"kind"`
			Requires string `json:"requires"`
		} `json:"available"`
	} `json:"packages"`
	Rollout *struct {
		ID     string            `json:"id"`
		Target string            `json:"target"`
		Status string            `json:"status"`
		Error  string            `json:"error"`
		Notes  []string          `json:"notes"`
		Mods   map[string]string `json:"modules"`
	} `json:"rollout"`
}

func (a *App) printUpdates(v updateView) error {
	if a.output == "json" {
		return a.printJSON(v)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tVERSION\tSTATE")
	for _, n := range v.Nodes {
		state := "up"
		switch {
		case n.Down:
			state = "down"
		case n.Update != nil:
			state = n.Update.State
		}
		name := n.Name
		if n.Leader {
			name += " (leader)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, n.Version, state)
	}
	fmt.Fprintln(tw, "\t\t")
	fmt.Fprintln(tw, "PACKAGE\tINSTALLED\tAVAILABLE")
	for _, p := range v.Packages {
		var avail []string
		for _, c := range p.Available {
			s := c.Version + " (" + c.Kind + ")"
			if c.Kind == "major" {
				s = c.Version + " (major: by hand, see docs/updates.md)"
			}
			avail = append(avail, s)
		}
		inst := p.Installed
		if p.Mixed {
			inst += " (differs between nodes)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, inst, dash(strings.Join(avail, ", ")))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if v.CheckedAt.IsZero() {
		fmt.Fprintln(a.Stdout, "\nNot checked yet: run `db-branch update check`.")
	} else {
		fmt.Fprintf(a.Stdout, "\nChecked %s by %s.\n", v.CheckedAt.Local().Format(time.RFC3339), v.CheckedBy)
	}
	if r := v.Rollout; r != nil {
		fmt.Fprintf(a.Stdout, "Rollout %s to %s: %s\n", r.ID, r.Target, r.Status)
		if r.Error != "" {
			fmt.Fprintf(a.Stdout, "  error: %s\n", r.Error)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(a.Stdout, "  note: %s\n", n)
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) updateCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "update", Short: "Update the cluster to a newer minor version (a major update is done by hand)"}
	get := func(method, path string, in any) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var v updateView
			if err := c.Do(cmd.Context(), method, path, in, &v); err != nil {
				return err
			}
			return a.printUpdates(v)
		}
	}
	cmd.AddCommand(
		&cobra.Command{Use: "check", Short: "Ask the package repository for newer versions (the only step that uses the network)", Args: cobra.NoArgs,
			RunE: get("POST", "/v1/updates/check", nil)},
		&cobra.Command{Use: "status", Short: "Show node versions, newer versions on offer and the rollout", Args: cobra.NoArgs,
			RunE: get("GET", "/v1/updates", nil)},
		&cobra.Command{
			Use: "apply <version>", Short: "Update every node to a newer minor version, one at a time (followers first, the leader last)", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				if err := c.Do(cmd.Context(), "POST", "/v1/updates", map[string]any{"target": args[0]}, nil); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "Rollout to %s started. Each node restarts its agent when its turn comes; follow it with `db-branch update status`.\n", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use: "cancel", Short: "Stop the running rollout (nodes that have not started will not)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				return c.Do(cmd.Context(), "DELETE", "/v1/updates", nil, nil)
			},
		},
	)
	return cmd
}

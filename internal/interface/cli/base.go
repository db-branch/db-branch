package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type baseView struct {
	Name         string        `json:"name"`
	Runtime      string        `json:"runtime"`
	Status       string        `json:"status"`
	Reason       *string       `json:"status_reason"`
	Port         int           `json:"port"`
	Node         string        `json:"node"`
	ReplicaNodes *[]string     `json:"replica_nodes"`
	Anonymous    anonymousView `json:"anonymous"`
	Holders      []struct {
		Node   string `json:"node"`
		State  string `json:"state"`
		Origin bool   `json:"origin"`
		Reason string `json:"reason"`
	} `json:"holders"`
	LatestSnapshot *string `json:"latest_snapshot"`
	Snapshots      []struct {
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	} `json:"snapshots"`
}

func (a *App) baseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "base", Short: "Manage bases that branches are cloned from"}
	var noWait bool

	var runtime, dump, node, replicas string
	create := &cobra.Command{
		Use:   "create <name> --runtime <engine@version> [--dump file.sql.gz]",
		Short: "Create a base from a SQL dump and take its first snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
			body := map[string]any{"name": n.String(), "runtime": runtime, "dump": dump}
			if node != "" {
				body["node"] = node
			}
			if replicas != "" {
				nodes, err := parseReplicas(replicas)
				if err != nil {
					return err
				}
				body["replica_nodes"] = nodes
			}
			return a.start(cmd.Context(), c, "POST", "/v1/bases", body, noWait)
		}),
	}
	create.Flags().StringVar(&runtime, "runtime", "", "engine version, e.g. mysql@8.4.13 (required)")
	create.Flags().StringVar(&dump, "dump", "", "SQL dump on the node that creates the base (plain or .gz)")
	create.Flags().StringVar(&node, "node", "", "cluster node to create it on (default: this node)")
	create.Flags().StringVar(&replicas, "replicas", "", "nodes that keep a copy: all (default), origin, or node1,node2 (the origin always does)")

	snapshot := &cobra.Command{
		Use:   "snapshot <name>",
		Short: "Stop the base server cleanly and take a new snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
			return a.start(cmd.Context(), c, "POST", "/v1/bases/"+n.String()+"/snapshots", nil, noWait)
		}),
	}
	for _, c := range []*cobra.Command{create, snapshot} {
		c.Flags().BoolVar(&noWait, "no-wait", false, "print the operation ID and return immediately")
	}

	list := &cobra.Command{
		Use: "list", Short: "List bases", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []baseView `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/bases", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tRUNTIME\tNODE\tLATEST SNAPSHOT")
			for _, b := range out.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Name, b.Status, b.Runtime, b.Node, orDash(b.LatestSnapshot))
			}
			return tw.Flush()
		},
	}
	show := &cobra.Command{
		Use: "show <name>", Short: "Show a base and its snapshots", Args: cobra.ExactArgs(1),
		RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
			var b baseView
			if err := c.Do(cmd.Context(), "GET", "/v1/bases/"+n.String(), nil, &b); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(b)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "Name:\t%s\nStatus:\t%s\nRuntime:\t%s\nPort:\t%d\n", b.Name, b.Status, b.Runtime, b.Port)
			if b.Reason != nil {
				fmt.Fprintf(tw, "Reason:\t%s\n", *b.Reason)
			}
			fmt.Fprintf(tw, "Replicas:\t%s\n", replicasText(b.ReplicaNodes))
			fmt.Fprintf(tw, "Without sign-in:\t%s\n", anonymousText(b.Anonymous))
			for _, h := range b.Holders {
				role := h.State
				if h.Origin {
					role = "origin"
				} else if h.State == "skipped" && h.Reason != "" {
					role = "skipped (" + h.Reason + ")"
				}
				fmt.Fprintf(tw, "Holder:\t%s  %s\n", h.Node, role)
			}
			for _, s := range b.Snapshots {
				fmt.Fprintf(tw, "Snapshot:\t%s  %s\n", s.Name, s.CreatedAt)
			}
			return tw.Flush()
		}),
	}
	simple := func(use, short, method, suffix string) *cobra.Command {
		return &cobra.Command{
			Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1),
			RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
				return c.Do(cmd.Context(), method, "/v1/bases/"+n.String()+suffix, nil, nil)
			}),
		}
	}
	// base mask is hidden from the 1.0 CLI surface (DECISIONS.md 8-c).
	cmd.AddCommand(create, snapshot, list, show,
		simple("start", "Start the base server (to load more data before the next snapshot)", "POST", "/start"),
		simple("stop", "Stop the base server cleanly", "POST", "/stop"),
		simple("delete", "Delete a base no branch uses", "DELETE", ""),
		a.baseRefreshCommand(),
		a.baseReplicasCommand(),
		a.baseAnonymousCommand(),
	)
	return cmd
}

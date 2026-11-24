package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

// parseReplicas reads "all" (every node: nil), "origin" (the base's node
// only: empty list) or "node1,node2" (those nodes besides the origin).
func parseReplicas(s string) ([]string, error) {
	switch s = strings.TrimSpace(s); s {
	case "", "all":
		return nil, nil
	case "origin":
		return []string{}, nil
	}
	out := []string{}
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n == "" {
			return nil, fmt.Errorf("invalid replicas %q: use all, origin or node1,node2", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// replicasText is how a replica setting reads back.
func replicasText(nodes *[]string) string {
	switch {
	case nodes == nil:
		return "all nodes"
	case len(*nodes) == 0:
		return "origin only"
	}
	return "origin + " + strings.Join(*nodes, ", ")
}

func (a *App) baseReplicasCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "replicas <base> all|origin|node1,node2",
		Short: "Choose which nodes keep a copy of the base (the origin node always does)",
		Args:  cobra.ExactArgs(2),
		RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
			nodes, err := parseReplicas(cmd.Flags().Arg(1))
			if err != nil {
				return err
			}
			var b baseView
			if err := c.Do(cmd.Context(), "PUT", "/v1/bases/"+n.String()+"/replicas", map[string]any{"replica_nodes": nodes}, &b); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "base %s is kept on: %s\n", b.Name, replicasText(b.ReplicaNodes))
			return nil
		}),
	}
}

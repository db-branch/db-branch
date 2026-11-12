package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
)

type clusterView struct {
	Mode     string `json:"mode"`
	Leader   string `json:"leader"`
	Protocol int    `json:"protocol"`
	// ServiceIDs are the db-branch uid/gid the cluster uses.
	ServiceIDs domain.ServiceIDs `json:"service_ids"`
	Nodes      []struct {
		Name      string   `json:"name"`
		Host      string   `json:"host"`
		Version   string   `json:"version"`
		Leader    bool     `json:"leader"`
		Voter     bool     `json:"voter"`
		Down      bool     `json:"down"`
		LastSeen  *string  `json:"last_seen"`
		PoolSize  uint64   `json:"pool_size"`
		PoolFree  uint64   `json:"pool_free"`
		Runtimes  []string `json:"runtimes"`
		Protocol  int      `json:"protocol"`
		IDProblem string   `json:"id_problem"`
		System    struct {
			Distro string `json:"distro"`
			OS     string `json:"os"`
			Kernel string `json:"kernel"`
			ZFS    string `json:"zfs"`
		} `json:"system"`
	} `json:"nodes"`
}

func (a *App) clusterCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "Form and inspect a cluster of nodes"}
	withConfig := func(f func(cmd *cobra.Command, cfg config.Config, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			return f(cmd, cfg, args)
		}
	}
	var token string
	join := &cobra.Command{
		Use:   "join <member-address:7070> --token <token>",
		Short: "Join an existing cluster (run as root on the new node)",
		Args:  cobra.ExactArgs(1),
		RunE: withConfig(func(cmd *cobra.Command, cfg config.Config, args []string) error {
			if token == "" {
				return fmt.Errorf("--token is required (run `db-branch cluster token` on a member)")
			}
			if err := a.ClusterJoin(cmd.Context(), cfg, args[0], token); err != nil {
				return err
			}
			fmt.Fprintln(a.Stdout, "Joining; the agent restarted and will join shortly (as a non-voter first). Check: db-branch cluster status")
			return nil
		}),
	}
	join.Flags().StringVar(&token, "token", "", "join token from `db-branch cluster token`")
	alignIDs := &cobra.Command{
		Use:   "align-ids",
		Short: "Give this node's db-branch user and group the ids the cluster uses (run as root)",
		Long: "Stops db-branch and its database servers on this node, changes the db-branch user and group to the\n" +
			"uid/gid the cluster agreed on, re-owns the files db-branch owns here and restarts the agent. Refuses\n" +
			"(changing nothing) when another account on this node already has those ids. The agent must be running.",
		Args: cobra.NoArgs,
		RunE: withConfig(func(cmd *cobra.Command, cfg config.Config, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var v clusterView
			if err := c.Do(cmd.Context(), "GET", "/v1/cluster", nil, &v); err != nil {
				return err
			}
			if !v.ServiceIDs.Valid() {
				return fmt.Errorf("the cluster has not recorded its db-branch ids yet (it does so once every node is upgraded); nothing to align")
			}
			res, err := a.ClusterAlignIDs(cmd.Context(), cfg, v.ServiceIDs)
			if err != nil {
				return err
			}
			if !res.Changed {
				fmt.Fprintf(a.Stdout, "Already %s, as in the cluster.\n", res.To)
				return nil
			}
			fmt.Fprintf(a.Stdout, "Changed the db-branch user and group from %s to %s and restarted the agent.\n", res.From, res.To)
			return nil
		}),
	}
	cmd.AddCommand(
		alignIDs,
		&cobra.Command{
			Use:   "init",
			Short: "Turn this node into a one-node cluster others can join (run as root)",
			Args:  cobra.NoArgs,
			RunE: withConfig(func(cmd *cobra.Command, cfg config.Config, _ []string) error {
				if err := a.ClusterInit(cmd.Context(), cfg); err != nil {
					return err
				}
				fmt.Fprintln(a.Stdout, "Cluster created. Invite nodes with: db-branch cluster token")
				return nil
			}),
		},
		join,
		&cobra.Command{
			Use:   "token",
			Short: "Issue a join token (replaces the previous one)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				var out struct {
					Token string `json:"token"`
				}
				if err := c.Do(cmd.Context(), "POST", "/v1/cluster/token", nil, &out); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "%s\n\nOn the new node: sudo db-branch cluster join <this-node>:7070 --token %s\n", out.Token, out.Token)
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show members and the leader",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				var v clusterView
				if err := c.Do(cmd.Context(), "GET", "/v1/cluster", nil, &v); err != nil {
					return err
				}
				if a.output == "json" {
					return a.printJSON(v)
				}
				tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NODE\tHOST\tROLE\tVOTING\tSTATE\tPOOL FREE\tENGINES\tOS\tVERSION")
				for _, n := range v.Nodes {
					role, state, voting := "follower", "up", "non-voter"
					if n.Leader {
						role = "leader"
					}
					if n.Voter {
						voting = "voter"
					}
					if n.Down {
						state = "down"
					}
					engines := strings.Join(n.Runtimes, ",")
					if engines == "" {
						engines = "-"
					}
					distro := n.System.Distro
					if distro == "" {
						distro = "unknown"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Host, role, voting, state, HumanBytes(n.PoolFree), engines, distro, n.Version)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if v.ServiceIDs.Valid() {
					fmt.Fprintf(a.Stdout, "\nThe cluster runs db-branch as %s.\n", v.ServiceIDs)
				}
				for _, n := range v.Nodes {
					if n.IDProblem != "" {
						fmt.Fprintf(a.Stdout, "\nPROBLEM on %s: %s\nIt takes no new branches or replicas until fixed.\n", n.Name, n.IDProblem)
					}
				}
				for _, n := range v.Nodes {
					if n.Protocol > v.Protocol {
						fmt.Fprintf(a.Stdout, "\nUpgrading: the cluster runs protocol %d until every node is upgraded; newer features wait until then.\n", v.Protocol)
						break
					}
				}
				return nil
			},
		},
	)
	return cmd
}

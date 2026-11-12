package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type branchView struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Reason     *string `json:"status_reason"`
	Base       string  `json:"base"`
	Snapshot   string  `json:"snapshot"`
	Runtime    string  `json:"runtime"`
	Node       string  `json:"node"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	ResetAt    *string `json:"reset_at"`
	Owner      *string `json:"owner"`
	ID         string  `json:"id"`
	Access     string  `json:"access"`
	Connection struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		User string `json:"user"`
	} `json:"connection"`
	Storage *struct {
		Used       uint64 `json:"used"`
		Referenced uint64 `json:"referenced"`
	} `json:"storage"`
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// HumanBytes formats a byte count with binary units.
func HumanBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v, i := float64(n)/1024, 0
	for v >= 1024 && i < len(units)-1 {
		v, i = v/1024, i+1
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// named runs f with a validated name argument and a client. "owner/name",
// as lists show branches, means name.
func (a *App) named(f func(cmd *cobra.Command, c Client, n domain.Name) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		arg := args[0]
		if i := strings.LastIndex(arg, "/"); i >= 0 {
			arg = arg[i+1:]
		}
		n, err := domain.ParseName(arg)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		return f(cmd, c, n)
	}
}

func (a *App) branchCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Aliases: []string{"br"}, Short: "Create, reset and delete branches"}
	var noWait bool

	var base, snapshot, initSQL, initFile, ttl, node, owner, access string
	var record bool
	create := &cobra.Command{
		Use:   "create <name> --base <base>",
		Short: "Clone a base into a new branch",
		Args:  cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			if _, err := domain.ParseName(base); err != nil {
				return fmt.Errorf("--base: %w", err)
			}
			if access != "" && !domain.ValidAccess(access) {
				return fmt.Errorf("--access: %q is neither %s nor %s", access, domain.AccessPrivate, domain.AccessOpen)
			}
			sql := initSQL
			if initFile != "" {
				b, err := os.ReadFile(initFile)
				if err != nil {
					return err
				}
				sql = string(b)
			}
			body := map[string]any{"name": n.String(), "base": base, "init_sql": sql, "ttl": ttl}
			if snapshot != "" {
				body["snapshot"] = snapshot
			}
			if node != "" {
				body["node"] = node
			}
			if owner != "" {
				body["owner"] = owner
			}
			if access != "" {
				body["access"] = access
			}
			if record {
				body["record"] = true
			}
			if err := a.start(cmd.Context(), c, "POST", "/v1/branches", body, noWait); err != nil || noWait {
				return err
			}
			return a.showBranch(cmd, c, n)
		}),
	}
	create.Flags().StringVar(&base, "base", "", "base to clone (required)")
	create.Flags().StringVar(&snapshot, "snapshot", "", "base snapshot (default: latest)")
	create.Flags().StringVar(&initSQL, "init-sql", "", "SQL to run once after cloning (e.g. masking)")
	create.Flags().StringVar(&initFile, "init-file", "", "read --init-sql from a file")
	create.Flags().StringVar(&ttl, "ttl", "", "delete automatically after this long, e.g. 72h")
	create.Flags().StringVar(&node, "node", "", "cluster node to create it on (default: chosen by load)")
	create.Flags().StringVar(&owner, "owner", "", "who manages the branch (admins only; default: you)")
	create.Flags().BoolVar(&record, "record", false, "record the branch's changes so it can be restored to any point (needs the history module)")
	create.Flags().StringVar(&access, "access", "", "private (own password, owner and admins only) or open (anyone, shared password)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List branches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []branchView `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/branches", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tBASE\tRUNTIME\tNODE\tPORT\tUSED\tEXPIRES")
			for _, b := range out.Items {
				used := "-"
				if b.Storage != nil {
					used = HumanBytes(b.Storage.Used)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", orValue(b.ID, b.Name), b.Status, b.Base, b.Runtime, b.Node, b.Connection.Port, used, orDash(b.ExpiresAt))
			}
			return tw.Flush()
		},
	}

	show := &cobra.Command{
		Use: "show <name>", Short: "Show a branch", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error { return a.showBranch(cmd, c, n) }),
	}
	var viaProxy bool
	dsn := &cobra.Command{
		Use: "dsn <name>", Short: "Print the connection URL (includes the password)", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			var out struct {
				DSN string `json:"dsn"`
			}
			path := "/v1/branches/" + n.String() + "/dsn"
			if viaProxy {
				path += "?via=proxy"
			}
			if err := c.Do(cmd.Context(), "GET", path, nil, &out); err != nil {
				return err
			}
			fmt.Fprintln(a.Stdout, out.DSN)
			return nil
		}),
	}
	dsn.Flags().BoolVar(&viaProxy, "proxy", false, "through this node's proxy (user name selects the branch)")
	reset := &cobra.Command{
		Use: "reset <name>", Short: "Discard every change since the branch was created", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			return a.start(cmd.Context(), c, "POST", "/v1/branches/"+n.String()+"/reset", nil, noWait)
		}),
	}
	var recreateNode string
	recreate := &cobra.Command{
		Use:   "recreate <name>",
		Short: "Build a lost branch again on another node (same name and credentials, from the reset point)",
		Args:  cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			path := "/v1/branches/" + n.String() + "/recreate"
			if recreateNode != "" {
				path += "?node=" + url.QueryEscape(recreateNode)
			}
			if err := a.start(cmd.Context(), c, "POST", path, nil, noWait); err != nil || noWait {
				return err
			}
			return a.showBranch(cmd, c, n)
		}),
	}
	recreate.Flags().StringVar(&recreateNode, "node", "", "node to build it on (default: chosen by load)")
	del := &cobra.Command{
		Use: "delete <name>", Aliases: []string{"rm"}, Short: "Delete a branch", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			return a.start(cmd.Context(), c, "DELETE", "/v1/branches/"+n.String(), nil, noWait)
		}),
	}
	extend := &cobra.Command{
		Use: "extend <name> <ttl>", Short: "Push the expiry to now + ttl", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
				var b branchView
				if err := c.Do(cmd.Context(), "PATCH", "/v1/branches/"+n.String(), map[string]any{"ttl": args[1]}, &b); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "%s expires at %s\n", n, orDash(b.ExpiresAt))
				return nil
			})(cmd, args)
		},
	}
	restore := &cobra.Command{
		Use:   "restore <name> <checkpoint>",
		Short: "Put a branch back to a checkpoint (nothing is lost: see 'branch checkpoints')",
		Args:  cobra.ExactArgs(2),
		RunE:  a.restoreCheckpoint(&noWait),
	}
	for _, c := range []*cobra.Command{create, reset, recreate, del, restore} {
		c.Flags().BoolVar(&noWait, "no-wait", false, "print the operation ID and return immediately")
	}
	cmd.AddCommand(create, list, show, dsn, reset, recreate, del, extend, a.checkpointCommand(), a.checkpointsCommand(), restore)
	return cmd
}

func (a *App) showBranch(cmd *cobra.Command, c Client, n domain.Name) error {
	var b branchView
	if err := c.Do(cmd.Context(), "GET", "/v1/branches/"+n.String(), nil, &b); err != nil {
		return err
	}
	if a.output == "json" {
		return a.printJSON(b)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", orValue(b.ID, b.Name))
	fmt.Fprintf(tw, "Status:\t%s\n", b.Status)
	if b.Access != "" {
		fmt.Fprintf(tw, "Access:\t%s\n", b.Access)
	}
	if b.Reason != nil {
		fmt.Fprintf(tw, "Reason:\t%s\n", *b.Reason)
	}
	fmt.Fprintf(tw, "Base:\t%s @ %s\n", b.Base, b.Snapshot)
	fmt.Fprintf(tw, "Runtime:\t%s\n", b.Runtime)
	fmt.Fprintf(tw, "Connect:\t%s:%d  user %s  (password: db-branch branch dsn %s)\n", b.Connection.Host, b.Connection.Port, b.Connection.User, b.Name)
	fmt.Fprintf(tw, "Expires:\t%s\n", orDash(b.ExpiresAt))
	if b.Storage != nil {
		fmt.Fprintf(tw, "Storage:\t%s own, %s total\n", HumanBytes(b.Storage.Used), HumanBytes(b.Storage.Referenced))
	}
	return tw.Flush()
}

func orValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

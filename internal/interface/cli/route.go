package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func (a *App) routeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "route", Short: "Route connections to branches through the proxy"}
	users := &cobra.Command{Use: "user", Short: "Users shared by every branch, routed by /*db_branch='name'*/ comments"}

	var def string
	add := &cobra.Command{
		Use:   "add <user> --default <branch> < password",
		Short: "Add or update a route user (password read from stdin; the user must exist in the branches, e.g. from the base dump)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			line, err := bufio.NewReader(a.Stdin).ReadString('\n')
			password := strings.TrimRight(line, "\r\n")
			if password == "" {
				return errors.Join(errors.New("give the password on stdin, e.g. `db-branch route user add app --default main < pw.txt`"), err)
			}
			body := map[string]any{"password": password, "default_branch": def}
			if err := c.Do(cmd.Context(), "PUT", "/v1/route-users/"+args[0], body, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "route user %s: default branch %s; pick another with /*db_branch='name'*/\n", args[0], def)
			return nil
		},
	}
	add.Flags().StringVar(&def, "default", "", "branch reached without a comment (required)")
	_ = add.MarkFlagRequired("default")

	list := &cobra.Command{
		Use: "list", Short: "List route users", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []struct {
					Name          string `json:"name"`
					DefaultBranch string `json:"default_branch"`
				} `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/route-users", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "USER\tDEFAULT BRANCH")
			for _, u := range out.Items {
				fmt.Fprintf(tw, "%s\t%s\n", u.Name, u.DefaultBranch)
			}
			return tw.Flush()
		},
	}
	remove := &cobra.Command{
		Use: "remove <user>", Short: "Remove a route user", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return c.Do(cmd.Context(), "DELETE", "/v1/route-users/"+args[0], nil, nil)
		},
	}
	users.AddCommand(add, list, remove)
	cmd.AddCommand(users)
	return cmd
}

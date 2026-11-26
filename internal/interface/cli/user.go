package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type cliUserView struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Disabled   bool   `json:"disabled"`
	MustChange bool   `json:"must_change_password"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func (a *App) userCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Manage console user accounts"}
	cmd.AddCommand(
		a.userAddCommand(),
		a.userListCommand(),
		a.userSetDisabledCommand("disable", "Disable a console user account", true),
		a.userSetDisabledCommand("enable", "Enable a disabled console user account", false),
		a.userRemoveCommand(),
		a.userResetPasswordCommand(),
	)
	return cmd
}

func (a *App) userAddCommand() *cobra.Command {
	var name string
	var passwordStdin, mustChange bool
	add := &cobra.Command{
		Use:   "add <email>",
		Short: "Create a console user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"email": args[0]}
			if name != "" {
				body["name"] = name
			}
			if passwordStdin {
				pw, err := a.readStdinLine()
				if err != nil {
					return err
				}
				body["password"] = pw
			}
			if cmd.Flags().Changed("must-change") {
				body["must_change_password"] = mustChange
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				User            cliUserView `json:"user"`
				InitialPassword string      `json:"initial_password,omitempty"`
			}
			if err := c.Do(cmd.Context(), "POST", "/v1/users", body, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out)
			}
			fmt.Fprintf(a.Stdout, "Created %s\n", out.User.ID)
			if out.InitialPassword != "" {
				fmt.Fprintf(a.Stdout, "Initial password (shown only once): %s\n", out.InitialPassword)
			}
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "display name (defaults to email)")
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read initial password from standard input")
	add.Flags().BoolVar(&mustChange, "must-change", true, "require password change at first sign-in")
	return add
}

func (a *App) userListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List console user accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Items []cliUserView `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/users", nil, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out.Items)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tSTATUS\tMUST CHANGE\tCREATED")
			for _, u := range out.Items {
				email := u.Email
				if email == "" {
					email = "-"
				}
				status := "active"
				if u.Disabled {
					status = "disabled"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\n", u.ID, email, u.Name, status, u.MustChange, u.CreatedAt)
			}
			return tw.Flush()
		},
	}
}

func (a *App) userSetDisabledCommand(use, short string, disabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id-or-email>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := domain.ResolveUserID(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var out cliUserView
			if err := c.Do(cmd.Context(), "PATCH", "/v1/users/"+url.PathEscape(id), map[string]bool{"disabled": disabled}, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out)
			}
			state := "Enabled"
			if disabled {
				state = "Disabled"
			}
			fmt.Fprintf(a.Stdout, "%s %s\n", state, id)
			return nil
		},
	}
}

func (a *App) userRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id-or-email>",
		Short: "Delete a console user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := domain.ResolveUserID(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			if err := c.Do(cmd.Context(), "DELETE", "/v1/users/"+url.PathEscape(id), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "Removed %s\n", id)
			return nil
		},
	}
}

func (a *App) userResetPasswordCommand() *cobra.Command {
	var mustChange bool
	cmd := &cobra.Command{
		Use:   "reset-password <id-or-email>",
		Short: "Set a new random password on a console user and end their sessions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := domain.ResolveUserID(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				User     cliUserView `json:"user"`
				Password string      `json:"password"`
			}
			if err := c.Do(cmd.Context(), "POST", "/v1/users/"+url.PathEscape(id)+"/reset-password", map[string]bool{"must_change_password": mustChange}, &out); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(out)
			}
			fmt.Fprintf(a.Stdout, "New password for %s (shown only once): %s\n", id, out.Password)
			return nil
		},
	}
	cmd.Flags().BoolVar(&mustChange, "must-change", true, "require password change at next sign-in")
	return cmd
}

func (a *App) readStdinLine() (string, error) {
	if a.Stdin == nil {
		return "", errors.New("standard input is unavailable")
	}
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("empty password on standard input")
	}
	return pw, nil
}

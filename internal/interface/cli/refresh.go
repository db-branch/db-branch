package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

// SourcePasswordEnv is the environment variable `base refresh set` reads the
// source database password from.
const SourcePasswordEnv = "DB_BRANCH_SOURCE_PASSWORD"

type refreshView struct {
	Base   string `json:"base"`
	Engine string `json:"engine"`
	Source struct {
		Host        string   `json:"host"`
		Port        int      `json:"port"`
		User        string   `json:"user"`
		PasswordSet bool     `json:"password_set"`
		Databases   []string `json:"databases"`
	} `json:"source"`
	Every   string `json:"every"`
	LastRun *struct {
		StartedAt  string  `json:"started_at"`
		FinishedAt string  `json:"finished_at"`
		OK         bool    `json:"ok"`
		Error      *string `json:"error"`
	} `json:"last_run"`
	NextRun string `json:"next_run"`
}

// sourcePassword reads the password from stdin (one line) or the
// environment; never from a flag, which would show in the process list.
func (a *App) sourcePassword(fromStdin bool) (string, error) {
	if fromStdin {
		if a.Stdin == nil {
			return "", errors.New("--password-stdin: no standard input")
		}
		line, err := bufio.NewReader(a.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return "", errors.New("--password-stdin: the password is empty")
		}
		return line, nil
	}
	return os.Getenv(SourcePasswordEnv), nil
}

func (a *App) baseRefreshCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "refresh", Short: "Reload a base from a source database on a schedule"}
	var host, user, databases, every string
	var port int
	var passwordStdin, noWait bool

	set := &cobra.Command{
		Use:   "set <base> --host H --port P --user U --databases a,b --every 24h",
		Short: "Refresh the base from a source database every interval (password: --password-stdin or $" + SourcePasswordEnv + "; none keeps the stored one)",
		Args:  cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			if host == "" || port == 0 || user == "" || databases == "" || every == "" {
				return errors.New("--host, --port, --user, --databases and --every are required")
			}
			password, err := a.sourcePassword(passwordStdin)
			if err != nil {
				return err
			}
			var dbs []string
			for _, d := range strings.Split(databases, ",") {
				dbs = append(dbs, strings.TrimSpace(d))
			}
			body := map[string]any{
				"source": map[string]any{"host": host, "port": port, "user": user, "password": password, "databases": dbs},
				"every":  every,
			}
			var v refreshView
			if err := c.Do(cmd.Context(), "PUT", "/v1/bases/"+n.String()+"/refresh", body, &v); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "base %s refreshes every %s from %s:%d; next run %s\n", v.Base, v.Every, v.Source.Host, v.Source.Port, v.NextRun)
			return nil
		}),
	}
	set.Flags().StringVar(&host, "host", "", "source database host")
	set.Flags().IntVar(&port, "port", 0, "source database port")
	set.Flags().StringVar(&user, "user", "", "source database user")
	set.Flags().StringVar(&databases, "databases", "", "comma-separated databases to copy")
	set.Flags().StringVar(&every, "every", "", "interval, e.g. 24h (at least 1h)")
	set.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the source password from standard input")

	show := &cobra.Command{
		Use: "show <base>", Short: "Show the base's refresh schedule and its last run", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			var v refreshView
			if err := c.Do(cmd.Context(), "GET", "/v1/bases/"+n.String()+"/refresh", nil, &v); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(v)
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			password := "not set"
			if v.Source.PasswordSet {
				password = "set"
			}
			fmt.Fprintf(tw, "Base:\t%s\nSource:\t%s://%s@%s:%d\nPassword:\t%s\nDatabases:\t%s\nEvery:\t%s\n",
				v.Base, v.Engine, v.Source.User, v.Source.Host, v.Source.Port, password, strings.Join(v.Source.Databases, ", "), v.Every)
			if r := v.LastRun; r != nil {
				result := "ok"
				if !r.OK {
					result = "failed: " + orDash(r.Error)
				}
				fmt.Fprintf(tw, "Last run:\t%s  %s\n", r.FinishedAt, result)
			} else {
				fmt.Fprintln(tw, "Last run:\t-")
			}
			fmt.Fprintf(tw, "Next run:\t%s\n", v.NextRun)
			return tw.Flush()
		}),
	}

	run := &cobra.Command{
		Use: "run <base>", Short: "Refresh the base now", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			return a.start(cmd.Context(), c, "POST", "/v1/bases/"+n.String()+"/refresh/run", nil, noWait)
		}),
	}
	run.Flags().BoolVar(&noWait, "no-wait", false, "print the operation ID and return immediately")

	off := &cobra.Command{
		Use: "off <base>", Short: "Stop refreshing the base on a schedule (forgets the source and its password)", Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			return c.Do(cmd.Context(), "DELETE", "/v1/bases/"+n.String()+"/refresh", nil, nil)
		}),
	}
	cmd.AddCommand(set, show, run, off)
	return cmd
}

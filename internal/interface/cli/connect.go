package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type branchProxy struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

type branchConnection struct {
	Branch   string       `json:"branch"`
	Engine   string       `json:"engine"`
	Host     string       `json:"host"`
	Port     int          `json:"port"`
	User     string       `json:"user"`
	Password string       `json:"password"`
	Database string       `json:"database,omitempty"`
	URL      string       `json:"url"`
	ViaProxy bool         `json:"via_proxy"`
	Proxy    *branchProxy `json:"proxy,omitempty"`
}

func (a *App) connectCommand() *cobra.Command {
	var proxy, reveal, includePassword, force bool
	var execClient, format, outPath string

	cmd := &cobra.Command{
		Use:   "connect <branch> [-- <extra-client-args>...]",
		Short: "Show connection details, launch mysql/psql, or generate a GUI client connection file",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if execClient != "" && format != "" {
				return errors.New("specify either --exec or --format, not both")
			}
			nameArg := args[0]
			if i := strings.LastIndex(nameArg, "/"); i >= 0 {
				nameArg = nameArg[i+1:]
			}
			n, err := domain.ParseName(nameArg)
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			conn, err := a.fetchBranchConnection(cmd, c, n, proxy)
			if err != nil {
				return err
			}
			switch {
			case execClient != "":
				return a.execBranchClient(conn, execClient, args[1:])
			case format != "":
				return a.writeClientFile(conn, format, outPath, includePassword, force)
			default:
				return a.printBranchConnection(conn, reveal)
			}
		},
	}
	cmd.Flags().BoolVar(&proxy, "proxy", false, "connect through ProxySQL")
	cmd.Flags().BoolVar(&reveal, "reveal", false, "show the password in table output")
	cmd.Flags().StringVar(&execClient, "exec", "", "launch an interactive DB client: mysql, psql, or auto")
	cmd.Flags().StringVar(&format, "format", "", "generate a client connection file/URL: dbeaver, chat2db, datagrip, navicat, pgadmin, workbench, tableplus")
	cmd.Flags().StringVar(&outPath, "out", "", "output file path for --format (use - for stdout)")
	cmd.Flags().BoolVar(&includePassword, "include-password", false, "include password in generated --format output when supported")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing --out file")
	return cmd
}

func (a *App) fetchBranchConnection(cmd *cobra.Command, c Client, n domain.Name, viaProxy bool) (branchConnection, error) {
	var b branchView
	if err := c.Do(cmd.Context(), "GET", "/v1/branches/"+n.String(), nil, &b); err != nil {
		return branchConnection{}, err
	}
	if b.Status != "" && b.Status != string(domain.BranchReady) {
		return branchConnection{}, fmt.Errorf("branch %s is %s (must be ready to connect)", n, b.Status)
	}
	dsnPath := "/v1/branches/" + n.String() + "/dsn"
	if viaProxy {
		dsnPath += "?via=proxy"
	}
	var dsnOut struct {
		DSN string `json:"dsn"`
	}
	if err := c.Do(cmd.Context(), "GET", dsnPath, nil, &dsnOut); err != nil {
		return branchConnection{}, err
	}
	u, err := url.Parse(dsnOut.DSN)
	if err != nil {
		return branchConnection{}, fmt.Errorf("parse branch DSN: %w", err)
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = b.Connection.Port
	}
	pw, _ := u.User.Password()
	var proxyInfo *branchProxy
	if b.Proxy != nil {
		proxyInfo = &branchProxy{Host: b.Proxy.Host, Port: b.Proxy.Port, User: b.Proxy.User}
	}
	return branchConnection{
		Branch:   n.String(),
		Engine:   normalizeEngineName(u.Scheme),
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: pw,
		Database: strings.TrimPrefix(u.Path, "/"),
		URL:      dsnOut.DSN,
		ViaProxy: viaProxy,
		Proxy:    proxyInfo,
	}, nil
}

func normalizeEngineName(e string) string {
	s := strings.ToLower(strings.TrimSpace(e))
	if s == "postgresql" {
		return "postgres"
	}
	return s
}

func maskDSN(rawURL, password string) string {
	if password == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	user := u.User.Username()
	u.User = nil
	// Avoid url.UserPassword escaping bullet mask characters.
	return strings.Replace(u.String(), "://", "://"+url.PathEscape(user)+":••••••@", 1)
}

func (a *App) printBranchConnection(conn branchConnection, reveal bool) error {
	if a.output == "json" {
		return a.printJSON(conn)
	}
	shownURL := conn.URL
	shownPW := conn.Password
	if !reveal {
		shownURL = maskDSN(conn.URL, conn.Password)
		shownPW = "•••••• (use --reveal to show)"
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Branch:\t%s\n", conn.Branch)
	fmt.Fprintf(tw, "Engine:\t%s\n", conn.Engine)
	fmt.Fprintf(tw, "Host:\t%s\n", conn.Host)
	fmt.Fprintf(tw, "Port:\t%d\n", conn.Port)
	fmt.Fprintf(tw, "User:\t%s\n", conn.User)
	fmt.Fprintf(tw, "Password:\t%s\n", shownPW)
	if conn.Database != "" {
		fmt.Fprintf(tw, "Database:\t%s\n", conn.Database)
	}
	fmt.Fprintf(tw, "URL:\t%s\n", shownURL)
	if conn.Proxy != nil {
		fmt.Fprintf(tw, "ProxySQL:\t%s  user %s\n", formatHostPort(conn.Proxy.Host, conn.Proxy.Port), conn.Proxy.User)
	}
	proxyFlag := ""
	if conn.ViaProxy {
		proxyFlag = " --proxy"
	}
	switch conn.Engine {
	case "mysql", "mariadb":
		fmt.Fprintf(tw, "Command:\tdb-branch connect %s%s --exec mysql\n", conn.Branch, proxyFlag)
	case "postgres":
		fmt.Fprintf(tw, "Command:\tdb-branch connect %s%s --exec psql\n", conn.Branch, proxyFlag)
	}
	return tw.Flush()
}

func resolveExecClient(engine, client string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(client))
	switch c {
	case "auto":
		switch engine {
		case "mysql", "mariadb":
			return "mysql", nil
		case "postgres":
			return "psql", nil
		default:
			return "", fmt.Errorf("--exec auto is not supported for engine %q", engine)
		}
	case "mysql":
		if engine != "mysql" && engine != "mariadb" {
			return "", fmt.Errorf("--exec mysql cannot be used with %s branch", engine)
		}
		return "mysql", nil
	case "psql":
		if engine != "postgres" {
			return "", fmt.Errorf("--exec psql cannot be used with %s branch", engine)
		}
		return "psql", nil
	default:
		return "", fmt.Errorf("unsupported --exec client %q (supported: mysql, psql, auto)", client)
	}
}

func checkNoControl(vals ...string) error {
	for _, v := range vals {
		if strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("connection parameter contains invalid control characters")
		}
	}
	return nil
}

func escapeMySQLOption(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func escapePGPassField(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	return r.Replace(s)
}

func (a *App) execBranchClient(conn branchConnection, client string, extraArgs []string) error {
	bin, err := resolveExecClient(conn.Engine, client)
	if err != nil {
		return err
	}
	if err := checkNoControl(conn.Host, conn.User, conn.Password, conn.Database); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "db-branch-conn-*")
	if err != nil {
		return fmt.Errorf("create temp credential dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var args []string
	var env []string
	if bin == "mysql" {
		cnfPath := filepath.Join(tmpDir, "my.cnf")
		cnf := fmt.Sprintf("[client]\nhost=%s\nport=%d\nuser=%s\npassword=%s\n",
			escapeMySQLOption(conn.Host), conn.Port, escapeMySQLOption(conn.User), escapeMySQLOption(conn.Password))
		if err := os.WriteFile(cnfPath, []byte(cnf), 0o600); err != nil {
			return err
		}
		env = filterEnv(os.Environ(), "MYSQL_PWD")
		args = append(args, "--defaults-extra-file="+cnfPath)
		args = append(args, extraArgs...)
		if conn.Database != "" {
			args = append(args, conn.Database)
		}
	} else {
		pgpassPath := filepath.Join(tmpDir, "pgpass")
		dbField := "*"
		if conn.Database != "" {
			dbField = escapePGPassField(conn.Database)
		}
		line := fmt.Sprintf("%s:%d:%s:%s:%s\n",
			escapePGPassField(conn.Host), conn.Port, dbField, escapePGPassField(conn.User), escapePGPassField(conn.Password))
		if err := os.WriteFile(pgpassPath, []byte(line), 0o600); err != nil {
			return err
		}
		env = filterEnv(os.Environ(), "PGPASSFILE", "PGPASSWORD")
		env = append(env, "PGPASSFILE="+pgpassPath)
		args = append(args, "-h", conn.Host, "-p", strconv.Itoa(conn.Port), "-U", conn.User)
		if conn.Database != "" {
			args = append(args, "-d", conn.Database)
		}
		args = append(args, extraArgs...)
	}

	runner := a.ExecCommand
	if runner == nil {
		runner = runInteractiveCommand
	}
	code, runErr := runner(bin, args, env, a.Stdin, a.Stdout, a.Stderr)
	if runErr != nil || code != 0 {
		if code <= 0 {
			code = 1
		}
		if runErr == nil {
			runErr = fmt.Errorf("%s exited with code %d", bin, code)
		}
		return &ExitError{Code: code, Err: runErr}
	}
	return nil
}

func filterEnv(environ []string, strip ...string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		k, _, _ := strings.Cut(entry, "=")
		skip := false
		for _, s := range strip {
			if strings.EqualFold(k, s) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, entry)
		}
	}
	return out
}

func runInteractiveCommand(name string, args []string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(env) > 0 {
		cmd.Env = env
	}
	// Trap SIGINT in db-branch via Go's handler (NOT signal.Ignore, which sets SIG_IGN
	// across fork/exec) so the child resets SIGINT to SIG_DFL and handles Ctrl+C while
	// db-branch stays alive until the child exits and cleans up temp credentials.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), err
		}
		return 1, err
	}
	return 0, nil
}

func formatHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

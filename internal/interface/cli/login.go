package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
	"github.com/db-branch/db-branch/internal/infrastructure/remotehttp"
)

func (a *App) readSecretInput() (string, error) {
	if a.Stdin == nil {
		return "", errors.New("cannot read token: no stdin")
	}
	if a.ReadSecret != nil {
		return a.ReadSecret(a.Stdin)
	}
	return termReadSecret(a.Stdin)
}

func (a *App) confirmYN(prompt string) (bool, error) {
	if !a.isTTY(a.Stdin) {
		return false, nil
	}
	fmt.Fprint(a.Stderr, prompt)
	line, err := readLine(a.Stdin)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func (a *App) resolveLoginServer(existing ProfileInfo, hasExisting, profExplicit bool) (string, error) {
	rawURL := firstNonEmpty(a.serverFlag, a.getenv("DB_BRANCH_URL"))
	if rawURL == "" && hasExisting && profExplicit {
		rawURL = existing.URL
	}
	if rawURL == "" {
		if !a.isTTY(a.Stdin) {
			return "", errors.New("--server <https://...> is required in non-interactive mode")
		}
		fmt.Fprint(a.Stderr, "Server URL (https://...): ")
		var err error
		rawURL, err = readLine(a.Stdin)
		if err != nil {
			return "", err
		}
	}
	u, err := remotehttp.ValidateServerURL(rawURL)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (a *App) readLoginToken(tokenFlag string, tokenStdin bool) (string, error) {
	if tokenFlag != "" && tokenStdin {
		return "", errors.New("specify either --token or --token-stdin, not both")
	}
	var token string
	switch {
	case tokenFlag != "":
		fmt.Fprintln(a.Stderr, "Warning: passing --token on the command line may expose it in process listings; prefer --token-stdin or interactive prompt.")
		token = strings.TrimSpace(tokenFlag)
	case tokenStdin:
		line, err := readLine(a.Stdin)
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		token = strings.TrimSpace(line)
	default:
		if !a.isTTY(a.Stdin) {
			return "", errors.New("API token required: pass --token-stdin (or --token) in non-interactive mode")
		}
		fmt.Fprint(a.Stderr, "API token: ")
		tok, err := a.readSecretInput()
		fmt.Fprintln(a.Stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		token = strings.TrimSpace(tok)
	}
	if token == "" {
		return "", errors.New("API token cannot be empty")
	}
	return token, nil
}

func (a *App) loginCommand() *cobra.Command {
	var tokenFlag, trustSHAFlag string
	var tokenStdin, noUse bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save a remote db-branch server URL and API token to a profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgStore, secStore, err := a.clientStores()
			if err != nil {
				return err
			}
			cfg, err := cfgStore.Load()
			if err != nil {
				return err
			}
			explicitProf := firstNonEmpty(a.profileFlag, a.getenv("DB_BRANCH_PROFILE"))
			profName := firstNonEmpty(explicitProf, "default")
			if err := clientcfg.ValidateProfileName(profName); err != nil {
				return err
			}
			existing, hasExisting := cfg.Profiles[profName]
			serverURL, err := a.resolveLoginServer(existing, hasExisting, explicitProf != "")
			if err != nil {
				return err
			}
			token, err := a.readLoginToken(tokenFlag, tokenStdin)
			if err != nil {
				return err
			}
			caCert := firstNonEmpty(a.caFlag, a.getenv("DB_BRANCH_CA"))
			trustPin := firstNonEmpty(trustSHAFlag, a.getenv("DB_BRANCH_TLS_SHA256"))
			if caCert != "" && trustPin != "" {
				return errors.New("specify either --ca or --trust-sha256, not both")
			}
			if trustPin != "" {
				if trustPin, err = remotehttp.NormalizeSHA256(trustPin); err != nil {
					return err
				}
			}
			pinnedSHA, err := a.resolveLoginTLS(cmd.Context(), serverURL, caCert, trustPin, existing, hasExisting)
			if err != nil {
				return err
			}
			c, err := a.buildRemoteClient(RemoteOptions{
				URL: serverURL, Token: token, CACert: caCert, PinnedSHA256: pinnedSHA,
			})
			if err != nil {
				return err
			}
			var caps struct {
				Version string `json:"version"`
				Node    string `json:"node"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/capabilities", nil, &caps); err != nil {
				return fmt.Errorf("login verification failed: %w", err)
			}
			if strings.TrimSpace(caps.Version) == "" {
				return errors.New("login verification failed: server did not return a valid db-branch /v1/capabilities payload")
			}
			// Also verify an auth-required endpoint so an unauthenticated /v1/capabilities cannot pass bad tokens.
			var branchCheck struct {
				Items []any `json:"items"`
			}
			if err := c.Do(cmd.Context(), "GET", "/v1/branches", nil, &branchCheck); err != nil {
				return fmt.Errorf("login authentication check failed: %w", err)
			}

			storage, warn, err := secStore.Save(profName, token)
			if err != nil {
				return fmt.Errorf("save token: %w", err)
			}
			if warn != "" {
				fmt.Fprintf(a.Stderr, "Warning: %s\n", warn)
			}
			if cfg.Profiles == nil {
				cfg.Profiles = map[string]ProfileInfo{}
			}
			cfg.Profiles[profName] = ProfileInfo{
				URL: serverURL, CACert: caCert, TLSSHA256: pinnedSHA, TokenStorage: storage,
			}
			if !noUse || cfg.CurrentProfile == "" {
				cfg.CurrentProfile = profName
			}
			if err := cfgStore.Save(cfg); err != nil {
				return err
			}
			if a.output == "json" {
				return a.printJSON(map[string]any{
					"profile": profName, "url": serverURL, "node": caps.Node,
					"version": caps.Version, "tls_sha256": pinnedSHA,
					"token_storage": storage, "current": cfg.CurrentProfile == profName,
				})
			}
			fmt.Fprintf(a.Stdout, "Logged in to %s (profile %q, node %s, version %s, token storage: %s)\n",
				serverURL, profName, caps.Node, caps.Version, storage)
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFlag, "token", "", "API token (insecure in process lists; prefer --token-stdin or prompt)")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read API token from standard input")
	cmd.Flags().StringVar(&trustSHAFlag, "trust-sha256", "", "trust a self-signed server certificate matching this SHA-256 fingerprint")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "do not switch current_profile to this profile")
	return cmd
}

func (a *App) resolveLoginTLS(ctx context.Context, serverURL, caCert, trustPin string, existing ProfileInfo, hasExisting bool) (string, error) {
	if a.ProbeTLS == nil {
		return trustPin, nil
	}
	if caCert != "" {
		_, err := a.ProbeTLS(ctx, serverURL, caCert, "")
		return "", err
	}
	if trustPin != "" {
		info, err := a.ProbeTLS(ctx, serverURL, "", trustPin)
		if err != nil {
			return "", err
		}
		return firstNonEmpty(info.SHA256, trustPin), nil
	}
	_, err := a.ProbeTLS(ctx, serverURL, "", "")
	if err == nil {
		return "", nil
	}
	var untrusted *UntrustedCertError
	if !errors.As(err, &untrusted) {
		return "", err
	}
	cert := untrusted.Cert
	if hasExisting && existing.TLSSHA256 != "" {
		if strings.EqualFold(existing.TLSSHA256, cert.SHA256) {
			return cert.SHA256, nil
		}
		fmt.Fprintf(a.Stderr, "\nWARNING: Server certificate changed for %s!\n  Previous SHA-256: %s\n  New SHA-256:      %s\n",
			serverURL, existing.TLSSHA256, cert.SHA256)
	}
	printUntrustedCert(a.Stderr, serverURL, cert)
	if !a.isTTY(a.Stdin) {
		return "", fmt.Errorf("untrusted TLS certificate (sha256:%s); verify the fingerprint on the server and re-run with --trust-sha256 %s or --ca <pem>",
			cert.SHA256, cert.SHA256)
	}
	ok, cerr := a.confirmYN("Trust this certificate and save its SHA-256 fingerprint? [y/N]: ")
	if cerr != nil {
		return "", cerr
	}
	if !ok {
		return "", errors.New("certificate not trusted")
	}
	return cert.SHA256, nil
}

func printUntrustedCert(w io.Writer, serverURL string, cert CertSummary) {
	fmt.Fprintf(w, "\nServer TLS certificate for %s is not trusted by system root CAs:\n", serverURL)
	fmt.Fprintf(w, "  Subject:    %s\n", cert.Subject)
	fmt.Fprintf(w, "  Issuer:     %s\n", cert.Issuer)
	fmt.Fprintf(w, "  Valid:      %s to %s\n", cert.NotBefore.UTC().Format(time.RFC3339), cert.NotAfter.UTC().Format(time.RFC3339))
	if len(cert.SANs) > 0 {
		fmt.Fprintf(w, "  SANs:       %s\n", strings.Join(cert.SANs, ", "))
	}
	fmt.Fprintf(w, "  SHA-256:    %s\n", cert.SHA256)
	if cert.SANMismatch {
		fmt.Fprintln(w, "  Warning:    certificate SAN does not match the server host/IP.")
	}
	fmt.Fprintln(w, "Verify on the server: openssl x509 -in /var/lib/db-branch/tls/cert.pem -noout -fingerprint -sha256")
}

func (a *App) logoutCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove saved local API token(s) from the keychain or credentials file",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfgStore, secStore, err := a.clientStores()
			if err != nil {
				return err
			}
			cfg, err := cfgStore.Load()
			if err != nil {
				return err
			}
			var targets []string
			if all {
				for name := range cfg.Profiles {
					targets = append(targets, name)
				}
				sort.Strings(targets)
			} else {
				name := firstNonEmpty(a.profileFlag, a.getenv("DB_BRANCH_PROFILE"), cfg.CurrentProfile)
				if name == "" {
					return errors.New("no active profile; specify --profile <name> or --all")
				}
				targets = []string{name}
			}
			for _, name := range targets {
				if err := secStore.Delete(name); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "Removed local credentials for profile %q.\n", name)
			}
			fmt.Fprintln(a.Stderr, "Note: server-side API tokens are not revoked automatically; revoke them on the server if no longer needed.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove local credentials for all profiles")
	return cmd
}

func (a *App) withProfileTarget(arg string, fn func(ClientConfigStore, SecretStore, *ClientConfigData, string) error) error {
	if err := clientcfg.ValidateProfileName(arg); err != nil {
		return err
	}
	cfgStore, secStore, err := a.clientStores()
	if err != nil {
		return err
	}
	cfg, err := cfgStore.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Profiles[arg]; !ok {
		return fmt.Errorf("profile %q does not exist", arg)
	}
	return fn(cfgStore, secStore, &cfg, arg)
}

func (a *App) profileCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Manage saved remote server profiles"}

	list := &cobra.Command{
		Use: "list", Short: "List saved profiles", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfgStore, _, err := a.clientStores()
			if err != nil {
				return err
			}
			cfg, err := cfgStore.Load()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Profiles))
			for n := range cfg.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			if a.output == "json" {
				type item struct {
					Name         string `json:"name"`
					Current      bool   `json:"current"`
					URL          string `json:"url"`
					TLS          string `json:"tls"`
					TokenStorage string `json:"token_storage,omitempty"`
				}
				out := make([]item, 0, len(names))
				for _, n := range names {
					p := cfg.Profiles[n]
					out = append(out, item{
						Name: n, Current: n == cfg.CurrentProfile, URL: p.URL,
						TLS: profileTLSMode(p), TokenStorage: p.TokenStorage,
					})
				}
				return a.printJSON(out)
			}
			if len(names) == 0 {
				fmt.Fprintln(a.Stdout, "No profiles configured. Run: db-branch login --server https://<host>:7080")
				return nil
			}
			tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PROFILE\tURL\tTLS\tTOKEN")
			for _, n := range names {
				p := cfg.Profiles[n]
				label := n
				if n == cfg.CurrentProfile {
					label = "* " + n
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", label, p.URL, profileTLSMode(p), orValue(p.TokenStorage, "-"))
			}
			return tw.Flush()
		},
	}

	use := &cobra.Command{
		Use: "use <name>", Short: "Switch the active profile", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.withProfileTarget(args[0], func(cfgStore ClientConfigStore, _ SecretStore, cfg *ClientConfigData, name string) error {
				cfg.CurrentProfile = name
				if err := cfgStore.Save(*cfg); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "Active profile set to %q.\n", name)
				return nil
			})
		},
	}

	remove := &cobra.Command{
		Use: "remove <name>", Aliases: []string{"rm", "delete"},
		Short: "Delete a saved profile and its local token", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.withProfileTarget(args[0], func(cfgStore ClientConfigStore, secStore SecretStore, cfg *ClientConfigData, name string) error {
				if err := secStore.Delete(name); err != nil {
					return err
				}
				delete(cfg.Profiles, name)
				if cfg.CurrentProfile == name {
					cfg.CurrentProfile = ""
				}
				if err := cfgStore.Save(*cfg); err != nil {
					return err
				}
				fmt.Fprintf(a.Stdout, "Removed profile %q.\n", name)
				return nil
			})
		},
	}

	cmd.AddCommand(list, use, remove)
	return cmd
}

func profileTLSMode(p ProfileInfo) string {
	switch {
	case p.CACert != "":
		return "ca"
	case p.TLSSHA256 != "":
		return "pinned"
	default:
		return "system"
	}
}

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

const (
	exitCodeUsage    = 2
	exitCodeUntrust  = 3
	exitCodeConflict = 4

	trustedKeysDir = "/etc/db-branch/trusted-keys"
)

// RuntimeModuleService is the usecase surface used by local runtime module CLI commands.
// *rtmodule.Service satisfies this interface.
type RuntimeModuleService interface {
	Install(ctx context.Context, req rtmodule.InstallRequest) (rtmodule.InstallResult, error)
	List() []rtmodule.ModuleInfo
	Remove(ctx context.Context, engine string, noRestart bool) (rtmodule.RemoveResult, error)
	TrustAdd(keyID string, pubRaw []byte, expected domain.Fingerprint) (rtmodule.TrustKey, error)
	TrustList() ([]rtmodule.TrustKey, error)
	TrustRemove(ctx context.Context, keyID string) error
}

func exitError(code int, err error) error {
	return &ExitError{Code: code, Err: err}
}

func mapRuntimeModuleError(err error) error {
	var existing *ExitError
	if errors.As(err, &existing) {
		return err
	}
	switch {
	case errors.Is(err, domain.ErrInvalidRef), errors.Is(err, domain.ErrInvalidModule):
		return exitError(exitCodeUsage, err)
	case errors.Is(err, domain.ErrKeyUntrusted), errors.Is(err, domain.ErrKeyMismatch):
		return exitError(exitCodeUntrust, err)
	case errors.Is(err, domain.ErrAlreadyInstalled),
		errors.Is(err, domain.ErrDowngradeRefused),
		errors.Is(err, domain.ErrInUse),
		errors.Is(err, domain.ErrKeyExists),
		errors.Is(err, rtmodule.ErrSourceMismatch),
		errors.Is(err, domain.ErrConflict):
		return exitError(exitCodeConflict, err)
	default:
		return err
	}
}

func (a *App) euid() int {
	if a.EffectiveUID != nil {
		return a.EffectiveUID()
	}
	return os.Geteuid()
}

func (a *App) requireRoot(op string) error {
	if a.euid() != 0 {
		return fmt.Errorf("%s requires root privileges (run with sudo)", op)
	}
	return nil
}

func (a *App) isTTY(r io.Reader) bool {
	if a.IsTTY != nil {
		return a.IsTTY(r)
	}
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (a *App) rtService(prompter rtmodule.TrustPrompter) (RuntimeModuleService, error) {
	if a.RuntimeModules == nil {
		return nil, errors.New("local runtime module management is not configured")
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	return a.RuntimeModules(cfg, prompter), nil
}

type cliTrustPrompter struct {
	app *App
}

func (p cliTrustPrompter) ConfirmTrust(ctx context.Context, keyID string, fingerprint domain.Fingerprint, _ domain.GitHubRef, tag string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !p.app.isTTY(p.app.Stdin) {
		return false, fmt.Errorf(
			"%w: signing key %q is not trusted (candidate fingerprint: %s).\nVerify the fingerprint out-of-band and re-run with:\n  --trust-key %s\nor register the key first with `sudo db-branch module trust add`",
			domain.ErrKeyUntrusted, keyID, fingerprint, fingerprint,
		)
	}
	keyPath := filepath.Join(trustedKeysDir, keyID+".pub")
	fmt.Fprintf(p.app.Stderr, "\nUntrusted signing key:\n"+
		"  Key ID:      %s\n"+
		"  Fingerprint: %s\n"+
		"  Source:      bundled candidate in release %s (NOT trusted automatically)\n\n"+
		"WARNING: This key is NOT in %s/.\n"+
		"Verify the fingerprint out-of-band (e.g., on the author's official website or documentation)\n"+
		"before trusting it.\n"+
		"Trust this key and save to %s? [y/N]: ",
		keyID, fingerprint, tag, trustedKeysDir, keyPath,
	)
	if p.app.Stdin == nil {
		return false, nil
	}
	line, err := bufio.NewReader(p.app.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func isRuntimeInstallTarget(target string) bool {
	return strings.HasPrefix(target, "github.com/") || strings.Contains(target, "/")
}

type rtInstallJSON struct {
	Engine          string   `json:"engine"`
	Version         string   `json:"version"`
	PreviousVersion string   `json:"previous_version,omitempty"`
	Tag             string   `json:"tag"`
	KeyID           string   `json:"key_id"`
	KeyFingerprint  string   `json:"key_fingerprint"`
	KeyAdded        bool     `json:"key_added"`
	Upgraded        bool     `json:"upgraded"`
	User            string   `json:"user"`
	UserCreated     bool     `json:"user_created"`
	UserUID         uint32   `json:"user_uid,omitempty"`
	UserGID         uint32   `json:"user_gid,omitempty"`
	Path            string   `json:"path"`
	Restarted       bool     `json:"restarted"`
	Warnings        []string `json:"warnings,omitempty"`
}

func (a *App) runRuntimeModuleInstall(ctx context.Context, target, trustKeyStr string, upgrade, noRestart bool) error {
	ref, err := domain.ParseGitHubRef(target)
	if err != nil {
		return exitError(exitCodeUsage, err)
	}
	var trustKey domain.Fingerprint
	if trustKeyStr != "" {
		trustKey, err = domain.ParseFingerprint(trustKeyStr)
		if err != nil {
			return exitError(exitCodeUsage, err)
		}
	}
	if err := a.requireRoot("module install"); err != nil {
		return err
	}
	svc, err := a.rtService(cliTrustPrompter{app: a})
	if err != nil {
		return err
	}
	if a.output != "json" {
		if ref.Tag != "" {
			fmt.Fprintf(a.Stdout, "Fetching release %s from github.com/%s/%s ...\n", ref.Tag, ref.Owner, ref.Repo)
		} else {
			fmt.Fprintf(a.Stdout, "Fetching latest release from github.com/%s/%s ...\n", ref.Owner, ref.Repo)
		}
	}
	res, err := svc.Install(ctx, rtmodule.InstallRequest{
		Ref:       ref,
		Upgrade:   upgrade,
		NoRestart: noRestart,
		TrustKey:  trustKey,
	})
	if err != nil {
		return mapRuntimeModuleError(err)
	}
	modPath := filepath.Join(domain.ManifestRoot, res.Engine)
	if a.output == "json" {
		return a.printJSON(rtInstallJSON{
			Engine:          res.Engine,
			Version:         res.Version,
			PreviousVersion: res.PreviousVersion,
			Tag:             res.Tag,
			KeyID:           res.KeyID,
			KeyFingerprint:  res.KeyFingerprint.String(),
			KeyAdded:        res.KeyAdded,
			Upgraded:        res.Upgraded,
			User:            res.User,
			UserCreated:     res.UserCreated,
			UserUID:         res.UserUID,
			UserGID:         res.UserGID,
			Path:            modPath,
			Restarted:       res.Restarted,
			Warnings:        res.Warnings,
		})
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(a.Stderr, "Warning: %s\n", w)
	}
	if res.KeyAdded {
		fmt.Fprintf(a.Stdout, "\nTrusted key saved: %s\n", filepath.Join(trustedKeysDir, res.KeyID+".pub"))
		fmt.Fprintf(a.Stdout, "Verified signature (key_id=%s) and SHA-256 digests.\n", res.KeyID)
	} else {
		fmt.Fprintf(a.Stdout, "Verified signature (key_id=%s, trusted) and SHA-256 digests.\n", res.KeyID)
	}
	if res.UserCreated {
		fmt.Fprintf(a.Stdout, "Created system user: %s\n", res.User)
	} else {
		fmt.Fprintf(a.Stdout, "OS user %s already exists (uid=%d, gid=%d).\n", res.User, res.UserUID, res.UserGID)
	}
	action := "installed"
	if res.Upgraded {
		action = "upgraded"
		fmt.Fprintf(a.Stdout, "Upgraded runtime module %s %s -> %s in %s\n", res.Engine, res.PreviousVersion, res.Version, modPath)
	} else {
		fmt.Fprintf(a.Stdout, "Installed runtime module %s %s to %s\n", res.Engine, res.Version, modPath)
	}
	if res.Restarted {
		fmt.Fprintln(a.Stdout, "Restarted db-branch.service.")
	} else if noRestart {
		fmt.Fprintln(a.Stdout, "Skipped restarting db-branch.service (--no-restart). Restart it to load the module.")
	}
	pinnedRef := domain.GitHubRef{Owner: ref.Owner, Repo: ref.Repo, Tag: res.Tag}.String()
	cmdHint := fmt.Sprintf("sudo db-branch module install %s", pinnedRef)
	if res.Upgraded {
		cmdHint += " --upgrade"
	}
	if res.KeyFingerprint.String() != "" {
		cmdHint += " --trust-key " + res.KeyFingerprint.String()
	}
	fmt.Fprintf(a.Stdout, "\nNote: This %s the module on this node only.\nIn a cluster, run the same command with an explicit tag on every node:\n  %s\n", action, cmdHint)
	return nil
}

type rtModuleJSON struct {
	Engine    string `json:"engine"`
	Version   string `json:"version"`
	Signed    bool   `json:"signed"`
	KeyID     string `json:"key_id,omitempty"`
	RunAsUser string `json:"run_as_user,omitempty"`
	Status    string `json:"status"`
	Warning   string `json:"warning,omitempty"`
	Error     string `json:"error,omitempty"`
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) printRuntimeModulesTable(mods []rtmodule.ModuleInfo) error {
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ENGINE\tVERSION\tSIGNED\tKEY ID\tRUN AS USER\tSTATUS")
	for _, m := range mods {
		signed := "no"
		if m.Signed {
			signed = "yes"
		}
		status := m.Status
		if m.Err != nil {
			status = fmt.Sprintf("%s (%v)", status, m.Err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			dashIfEmpty(m.Engine),
			dashIfEmpty(m.Version),
			signed,
			dashIfEmpty(m.KeyID),
			dashIfEmpty(m.RunAsUser),
			status,
		)
	}
	return tw.Flush()
}

func (a *App) runRuntimeModuleList() error {
	svc, err := a.rtService(nil)
	if err != nil {
		return err
	}
	mods := svc.List()
	if a.output == "json" {
		items := make([]rtModuleJSON, 0, len(mods))
		for _, m := range mods {
			var errStr string
			if m.Err != nil {
				errStr = m.Err.Error()
			}
			items = append(items, rtModuleJSON{
				Engine:    m.Engine,
				Version:   m.Version,
				Signed:    m.Signed,
				KeyID:     m.KeyID,
				RunAsUser: m.RunAsUser,
				Status:    m.Status,
				Warning:   m.Warning,
				Error:     errStr,
			})
		}
		return a.printJSON(items)
	}
	return a.printRuntimeModulesTable(mods)
}

type rtRemoveJSON struct {
	Engine    string `json:"engine"`
	Removed   bool   `json:"removed"`
	Restarted bool   `json:"restarted"`
	UserKept  string `json:"user_kept"`
	KeyIDKept string `json:"key_id_kept,omitempty"`
}

func (a *App) hasLocalRuntimeModule(name string) (bool, error) {
	if a.RuntimeModules == nil {
		return false, nil
	}
	svc, err := a.rtService(nil)
	if err != nil {
		return false, err
	}
	for _, m := range svc.List() {
		if m.Engine == name {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) runRuntimeModuleRemove(ctx context.Context, engine string, noRestart bool) error {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return exitError(exitCodeUsage, fmt.Errorf("%w: %q", err, engine))
	}
	if err := a.requireRoot("module remove"); err != nil {
		return err
	}
	svc, err := a.rtService(nil)
	if err != nil {
		return err
	}
	res, err := svc.Remove(ctx, engine, noRestart)
	if err != nil {
		return mapRuntimeModuleError(err)
	}
	if a.output == "json" {
		return a.printJSON(rtRemoveJSON{
			Engine:    res.Engine,
			Removed:   res.Removed,
			Restarted: res.Restarted,
			UserKept:  res.UserKept,
			KeyIDKept: res.KeyIDKept,
		})
	}
	fmt.Fprintf(a.Stdout, "Removed runtime module %s (%s).\n", res.Engine, filepath.Join(domain.ManifestRoot, res.Engine))
	if res.Restarted {
		fmt.Fprintln(a.Stdout, "Restarted db-branch.service.")
	} else if noRestart {
		fmt.Fprintln(a.Stdout, "Skipped restarting db-branch.service (--no-restart). Restart it to unload the module.")
	}
	fmt.Fprintf(a.Stdout, "Note: OS user %q was NOT deleted so ownership of any remaining data/backups is preserved.\n", res.UserKept)
	if res.KeyIDKept != "" {
		fmt.Fprintf(a.Stdout, "Note: Trusted key %q was kept in %s/ (remove with `sudo db-branch module trust remove %s` if no longer needed).\n",
			res.KeyIDKept, trustedKeysDir, res.KeyIDKept)
	}
	return nil
}

type trustKeyJSON struct {
	KeyID       string `json:"key_id"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Path        string `json:"path,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

const maxTrustPubInputBytes = 4 << 10

func (a *App) readPubKeyInput(src string) ([]byte, error) {
	if src == "-" {
		if a.Stdin == nil {
			return nil, errors.New("standard input is not available")
		}
		data, err := io.ReadAll(io.LimitReader(a.Stdin, maxTrustPubInputBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxTrustPubInputBytes {
			return nil, fmt.Errorf("public key input exceeds %d bytes", maxTrustPubInputBytes)
		}
		return data, nil
	}
	return os.ReadFile(src)
}

func (a *App) runTrustAdd(keyID, pubSource, expectedFP string) error {
	var fp domain.Fingerprint
	if expectedFP != "" {
		var err error
		fp, err = domain.ParseFingerprint(expectedFP)
		if err != nil {
			return exitError(exitCodeUsage, err)
		}
	}
	if err := a.requireRoot("module trust add"); err != nil {
		return err
	}
	pubRaw, err := a.readPubKeyInput(pubSource)
	if err != nil {
		return err
	}
	svc, err := a.rtService(nil)
	if err != nil {
		return err
	}
	tk, err := svc.TrustAdd(keyID, pubRaw, fp)
	if err != nil {
		return mapRuntimeModuleError(err)
	}
	keyPath := filepath.Join(trustedKeysDir, tk.KeyID+".pub")
	if a.output == "json" {
		return a.printJSON(trustKeyJSON{
			KeyID:       tk.KeyID,
			Fingerprint: tk.Fingerprint.String(),
			Path:        keyPath,
		})
	}
	fmt.Fprintf(a.Stdout, "Registered trusted key %q\n  Path:        %s\n  Fingerprint: %s\n",
		tk.KeyID, keyPath, tk.Fingerprint)
	return nil
}

func (a *App) runTrustList() error {
	svc, err := a.rtService(nil)
	if err != nil {
		return err
	}
	keys, err := svc.TrustList()
	if err != nil {
		return mapRuntimeModuleError(err)
	}
	if a.output == "json" {
		items := make([]trustKeyJSON, 0, len(keys))
		for _, k := range keys {
			status, errStr := "ok", ""
			if k.Err != nil {
				status, errStr = "error", k.Err.Error()
			}
			items = append(items, trustKeyJSON{
				KeyID:       k.KeyID,
				Fingerprint: k.Fingerprint.String(),
				Status:      status,
				Error:       errStr,
			})
		}
		return a.printJSON(items)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY ID\tFINGERPRINT\tSTATUS")
	for _, k := range keys {
		status := "ok"
		if k.Err != nil {
			status = fmt.Sprintf("error (%v)", k.Err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", dashIfEmpty(k.KeyID), dashIfEmpty(k.Fingerprint.String()), status)
	}
	return tw.Flush()
}

func (a *App) runTrustRemove(ctx context.Context, keyID string) error {
	if err := a.requireRoot("module trust remove"); err != nil {
		return err
	}
	svc, err := a.rtService(nil)
	if err != nil {
		return err
	}
	if err := svc.TrustRemove(ctx, keyID); err != nil {
		return mapRuntimeModuleError(err)
	}
	if a.output == "json" {
		return a.printJSON(map[string]any{"key_id": keyID, "removed": true})
	}
	fmt.Fprintf(a.Stdout, "Removed trusted key %q (%s).\n", keyID, filepath.Join(trustedKeysDir, keyID+".pub"))
	return nil
}

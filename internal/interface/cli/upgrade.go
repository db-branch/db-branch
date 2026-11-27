package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

type findingView struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type upgradeResultView struct {
	Branch     string        `json:"branch"`
	From       string        `json:"from"`
	To         string        `json:"to"`
	Status     string        `json:"status"`
	CheckOnly  bool          `json:"check_only"`
	Checkpoint string        `json:"checkpoint"`
	PreCheck   []findingView `json:"pre_check"`
	Log        []findingView `json:"log"`
	Error      string        `json:"error"`
	LogExcerpt string        `json:"log_excerpt"`
}

func printFindings(w io.Writer, title string, list []findingView) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(w, "%s\n", title)
	for _, f := range list {
		fmt.Fprintf(w, "  %-8s %s\n", f.Level, f.Message)
	}
}

func countLevel(list []findingView, level string) int {
	n := 0
	for _, f := range list {
		if f.Level == level {
			n++
		}
	}
	return n
}

// printUpgradeResult tells what an upgrade did and found. branch is the
// name to use in follow-up commands.
func printUpgradeResult(w io.Writer, branch string, r upgradeResultView) {
	printFindings(w, fmt.Sprintf("Pre-check: %s to %s", r.From, r.To), r.PreCheck)
	if len(r.Log) > 0 {
		printFindings(w, "Server log during the upgrade:", r.Log)
	}
	switch domain.UpgradeStatus(r.Status) {
	case domain.UpgradeChecked:
		if errs := countLevel(r.PreCheck, "error"); errs > 0 {
			fmt.Fprintf(w, "Checked only, nothing was changed. %d error(s) block the upgrade unless you add --force.\n", errs)
		} else {
			fmt.Fprintf(w, "Checked only, nothing was changed. Upgrade with: db-branch branch upgrade %s --runtime %s\n", branch, r.To)
		}
	case domain.UpgradeUpgraded:
		fmt.Fprintf(w, "Upgraded %s from %s to %s. The state before is saved as checkpoint %s; to go back: db-branch branch restore %s %s\n", branch, r.From, r.To, r.Checkpoint, branch, r.Checkpoint)
	case domain.UpgradeRolledBack:
		fmt.Fprintf(w, "The upgrade failed and %s was rolled back to %s (checkpoint %s, kept).\n", branch, r.From, r.Checkpoint)
	case domain.UpgradeFailed:
		fmt.Fprintf(w, "The upgrade failed and %s was left as it is, as asked or because the rollback failed. Go back with: db-branch branch restore %s %s\n", branch, branch, r.Checkpoint)
	case domain.UpgradeRefused, domain.UpgradeAborted:
		fmt.Fprintf(w, "Nothing was changed on %s.\n", branch)
	}
	if r.LogExcerpt != "" {
		fmt.Fprintf(w, "End of the server's error log:\n%s\n", indent(r.LogExcerpt))
	}
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n  ")
}

// reportUpgrade prints the result an operation carries (if any), as JSON or text.
func (a *App) reportUpgrade(branch string, op operationJSON) {
	if len(op.Result) == 0 {
		return
	}
	var r upgradeResultView
	if json.Unmarshal(op.Result, &r) != nil {
		return
	}
	if a.output == "json" {
		_ = a.printJSON(r)
		return
	}
	printUpgradeResult(a.Stdout, branch, r)
}

func (a *App) upgradeCommand(noWait *bool) *cobra.Command {
	var runtime string
	var checkOnly, noRollback, force bool
	cmd := &cobra.Command{
		Use:   "upgrade <name> --runtime <engine@version>",
		Short: "Move one branch to a newer database version (the base and other branches stay as they are)",
		Long: `Moves one branch to a newer runtime of its engine, for example mysql@8.0.46 to mysql@8.4.13
or postgres@18.6 to postgres@19.0 (via pg_upgrade).
A checkpoint (before-upgrade-<time>) is saved first and a failed upgrade rolls back to it by itself;
--no-rollback keeps the failed state for investigation. --check-only runs the pre-check and changes nothing.
Upgrades only: to go back, restore the before-upgrade checkpoint. The target (and for PostgreSQL major
upgrades, the current runtime as well) must be installed (db-branch runtime install <engine@version>).`,
		Args: cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			if _, err := parseRuntimeArg(runtime); err != nil {
				return fmt.Errorf("--runtime: %w", err)
			}
			body := map[string]any{"runtime": runtime, "check_only": checkOnly, "no_rollback": noRollback, "force": force}
			op, err := a.startOp(cmd.Context(), c, "POST", "/v1/branches/"+n.String()+"/upgrade", body, *noWait)
			if err != nil || *noWait {
				return err
			}
			a.reportUpgrade(n.String(), op)
			if op.State == "failed" {
				return fmt.Errorf("%s failed: %s", op.Kind, op.Error)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&runtime, "runtime", "", "target runtime, engine@version (required), e.g. mysql@8.4.13 or postgres@19.0")
	_ = cmd.MarkFlagRequired("runtime")
	cmd.Flags().BoolVar(&checkOnly, "check-only", false, "run the pre-check only; change nothing")
	cmd.Flags().BoolVar(&noRollback, "no-rollback", false, "keep a failed upgrade as it is instead of rolling back (restore the checkpoint to go back)")
	cmd.Flags().BoolVar(&force, "force", false, "upgrade although the pre-check found errors")
	return cmd
}

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/spf13/cobra"
)

func (a *App) mergeCommand(noWait *bool) *cobra.Command {
	var (
		line           int
		checkpoint     string
		dryRun         bool
		tables         string
		allowDDL       bool
		ignoreTriggers bool
	)
	cmd := &cobra.Command{
		Use:   "merge <branch>",
		Short: "Merge a history line or checkpoint into the live branch",
		Args:  cobra.ExactArgs(1),
		RunE: a.named(func(cmd *cobra.Command, c Client, n domain.Name) error {
			hasLine := cmd.Flags().Changed("line")
			hasCP := cmd.Flags().Changed("checkpoint") && checkpoint != ""
			if (hasLine && hasCP) || (!hasLine && !hasCP) {
				return fmt.Errorf("specify either --line or --checkpoint (not both)")
			}
			if hasLine && line < 0 {
				return fmt.Errorf("--line must be >= 0")
			}
			body := map[string]any{
				"dry_run":         dryRun,
				"allow_ddl":       allowDDL,
				"ignore_triggers": ignoreTriggers,
			}
			if hasLine {
				body["source_line"] = line
			} else {
				body["checkpoint"] = checkpoint
			}
			if tables != "" {
				var list []string
				for _, t := range strings.Split(tables, ",") {
					if t = strings.TrimSpace(t); t != "" {
						list = append(list, t)
					}
				}
				if len(list) > 0 {
					body["tables"] = list
				}
			}
			op, err := a.startOp(cmd.Context(), c, "POST", "/v1/branches/"+n.String()+"/merge", body, *noWait)
			if err != nil || *noWait {
				return err
			}
			var res domain.MergeResult
			if len(op.Result) > 0 {
				if err := json.Unmarshal(op.Result, &res); err != nil {
					return err
				}
				if a.output == "json" {
					_ = a.printJSON(res)
				} else {
					reportMerge(a.Stdout, res)
				}
			}
			if res.Status == domain.MergeConflicted || res.Code == "merge_conflict" {
				msg := op.Error
				if msg == "" {
					msg = fmt.Sprintf("merge conflict: %d conflict(s) detected", res.TotalConflicts)
				}
				return &ExitError{Code: 2, Err: errors.New(msg)}
			}
			if op.State == "failed" {
				return errors.New(op.Error)
			}
			return nil
		}),
	}
	cmd.Flags().IntVar(&line, "line", 0, "source history line number to merge from")
	cmd.Flags().StringVar(&checkpoint, "checkpoint", "", "source checkpoint name (or @reset) to merge from")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview diffs and conflicts without changing live data")
	cmd.Flags().StringVar(&tables, "tables", "", "comma-separated table list (db.table or table)")
	cmd.Flags().BoolVar(&allowDDL, "allow-ddl", false, "apply DDL changes found in the 3-way diff")
	cmd.Flags().BoolVar(&ignoreTriggers, "ignore-triggers", false, "allow merging tables with triggers")
	cmd.Flags().BoolVar(noWait, "no-wait", false, "print the operation ID and return immediately")
	return cmd
}

func reportMerge(w io.Writer, res domain.MergeResult) {
	src := fmt.Sprintf("line %d", res.SourceLine)
	if res.SourceCheckpoint != "" {
		src = "checkpoint " + res.SourceCheckpoint
	}
	for _, t := range res.Tables {
		line := fmt.Sprintf("  %s.%s: +%d ~%d -%d", t.Database, t.Table, t.Inserted, t.Updated, t.Deleted)
		if t.DDL != "" {
			line += " (DDL)"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	switch res.Status {
	case domain.MergePreview:
		_, _ = fmt.Fprintf(w, "Dry run (%s -> %s line %d; ancestor %s): +%d ~%d -%d (nothing was changed).\n",
			src, res.Branch, res.TargetLine, res.Ancestor, res.Inserted, res.Updated, res.Deleted)
	case domain.MergeMerged:
		_, _ = fmt.Fprintf(w, "Merged %s into %s (line %d; ancestor %s): +%d ~%d -%d.\n",
			src, res.Branch, res.TargetLine, res.Ancestor, res.Inserted, res.Updated, res.Deleted)
		if res.Checkpoint != "" {
			_, _ = fmt.Fprintf(w, "Pre-merge checkpoint: db-branch branch restore %s %s\n", res.Branch, res.Checkpoint)
		}
	case domain.MergeConflicted:
		_, _ = fmt.Fprintf(w, "Merge conflicted on %s (%d total):\n", res.Branch, res.TotalConflicts)
		for _, c := range res.Conflicts {
			_, _ = fmt.Fprintf(w, "  %s %s.%s key=%v %s\n", c.Type, c.Database, c.Table, c.Key, c.Message)
		}
		if res.Truncated {
			_, _ = fmt.Fprintf(w, "  ... truncated at %d conflicts (total %d)\n", len(res.Conflicts), res.TotalConflicts)
		}
	case domain.MergeRolledBack:
		_, _ = fmt.Fprintf(w, "Merge failed on %s and was rolled back.\n", res.Branch)
	case domain.MergeFailed:
		_, _ = fmt.Fprintf(w, "Merge failed on %s (%s).\n", res.Branch, res.Error)
	}
}

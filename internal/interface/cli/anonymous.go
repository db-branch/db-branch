package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/db-branch/db-branch/internal/domain"
)

// anonymousView is a base's limits for callers without a login: null
// inherits the cluster-wide setting.
type anonymousView struct {
	MaxBranches *int    `json:"max_branches"`
	MaxTTL      *string `json:"max_ttl"`
}

// anonymousText is how the limits read back.
func anonymousText(v anonymousView) string {
	branches := "inherits the cluster-wide cap"
	switch {
	case v.MaxBranches != nil && *v.MaxBranches == 0:
		branches = "none (disabled)"
	case v.MaxBranches != nil:
		branches = fmt.Sprintf("up to %d branches (and the cluster-wide cap)", *v.MaxBranches)
	}
	ttl := "the cluster-wide TTL"
	if v.MaxTTL != nil {
		ttl = *v.MaxTTL + " at most (and the cluster-wide TTL)"
	}
	return branches + "; TTL: " + ttl
}

// anonymousMax reads --max: a number of branches (0: none) or "inherit".
func anonymousMax(s string) (any, error) {
	if strings.TrimSpace(s) == "inherit" {
		return nil, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid --max %q: use a number (0 allows none) or inherit", s)
	}
	return n, nil
}

// anonymousTTL reads --ttl: a positive duration or "inherit".
func anonymousTTL(s string) (any, error) {
	if strings.TrimSpace(s) == "inherit" {
		return nil, nil
	}
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid --ttl %q: use a positive duration such as 4h, or inherit", s)
	}
	return strings.TrimSpace(s), nil
}

func (a *App) baseAnonymousCommand() *cobra.Command {
	var most, ttl string
	cmd := &cobra.Command{
		Use:   "anonymous <base> [--max <n|inherit>] [--ttl <duration|inherit>]",
		Short: "Show or set how many branches, and for how long, callers without a login may make from the base",
		Long: "Callers without a login can branch only from public snapshots; this sets the base's own limits on top of the cluster-wide ones " +
			"(settings branch.anonymous_max and branch.anonymous_ttl). --max 0 allows none from this base; inherit uses the cluster-wide setting alone. " +
			"Without flags it shows the current limits.",
		Args: cobra.ExactArgs(1),
		RunE: a.exactNamed(func(cmd *cobra.Command, c Client, n domain.Name) error {
			body := map[string]any{}
			if cmd.Flags().Changed("max") {
				v, err := anonymousMax(most)
				if err != nil {
					return err
				}
				body["max_branches"] = v
			}
			if cmd.Flags().Changed("ttl") {
				v, err := anonymousTTL(ttl)
				if err != nil {
					return err
				}
				body["max_ttl"] = v
			}
			var b struct {
				Anonymous anonymousView `json:"anonymous"`
			}
			method, path, in := "GET", "/v1/bases/"+n.String(), any(nil)
			if len(body) > 0 {
				method, path, in = "PATCH", path+"/anonymous", body
			}
			if err := c.Do(cmd.Context(), method, path, in, &b); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "base %s, without sign-in: %s\n", n, anonymousText(b.Anonymous))
			return nil
		}),
	}
	cmd.Flags().StringVar(&most, "max", "", "most branches callers without a login may hold from this base: a number (0: none) or inherit")
	cmd.Flags().StringVar(&ttl, "ttl", "", "longest life of their branches, e.g. 4h, or inherit")
	return cmd
}

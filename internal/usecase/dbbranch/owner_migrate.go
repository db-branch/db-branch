package dbbranch

import (
	"context"
	"errors"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// MigrateBranchOwners migrates legacy branches (Owner != "" and OwnerName == "")
// to immutable principal IDs with separate display OwnerName. Branches without
// an owner remain unowned (admin-only). Already-migrated branches are skipped.
func MigrateBranchOwners(ctx context.Context, store domain.Store, migrator domain.BranchOwnerMigrator, tokens domain.TokenStore) error {
	if migrator == nil {
		return nil
	}
	branches, err := store.ListBranches(ctx)
	if err != nil {
		return err
	}
	var pending []domain.Branch
	for _, b := range branches {
		if b.Owner != "" && b.OwnerName == "" {
			pending = append(pending, b)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	var tokenList []domain.APIToken
	if tokens != nil {
		tokenList, err = tokens.ListTokens(ctx)
		if err != nil {
			return err
		}
	}
	var errs []error
	for _, b := range pending {
		owner, ownerName, ok := resolveLegacyOwner(b, tokenList)
		if !ok {
			continue // stays a legacy name owner: matched by display name, never by a service token
		}
		if err := migrator.MigrateBranchOwner(ctx, b.Name, owner, ownerName); err != nil {
			if errors.Is(err, domain.ErrClusterUpgrading) {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func resolveLegacyOwner(b domain.Branch, tokens []domain.APIToken) (owner, ownerName string, ok bool) {
	// Built-in principals have stable IDs equal to their names; never let a
	// service token with a colliding name claim them.
	switch b.Owner {
	case "admin", "local", "anonymous":
		return b.Owner, b.Owner, true
	}
	// Already an immutable service token ID ("token:<id>").
	if id, ok := strings.CutPrefix(b.Owner, "token:"); ok {
		for _, t := range tokens {
			if t.ID == id && t.Name != "" {
				return b.Owner, t.Name, true
			}
		}
		return b.Owner, b.Owner, true
	}
	// Already matches a known personal token's immutable principal ID.
	for _, t := range tokens {
		if t.Owner != "" && t.Owner == b.Owner {
			if t.OwnerName != "" {
				return b.Owner, t.OwnerName, true
			}
			return b.Owner, b.Owner, true
		}
	}
	// Match display name against personal tokens and pre-existing service tokens.
	// Only migrate when a single unambiguous principal ID matches.
	candidates := map[string]bool{}
	for _, t := range tokens {
		if !t.CreatedAt.IsZero() && !b.CreatedAt.IsZero() && t.CreatedAt.After(b.CreatedAt) {
			continue // a token made after the branch cannot have been its owner
		}
		if t.Owner != "" {
			if t.OwnerName == b.Owner {
				candidates[t.Owner] = true
			}
			continue
		}
		if t.Name == b.Owner {
			candidates["token:"+t.ID] = true
		}
	}
	if len(candidates) == 1 {
		for id := range candidates {
			return id, b.Owner, true
		}
	}
	// Ambiguous: several principals could be the owner, so none of them may
	// claim it. Seal it (OwnerName set, Owner no ID): admin-only.
	if len(candidates) > 1 {
		return b.Owner, b.Owner, true
	}
	// Unresolved: leave the branch as a legacy name owner (matched by display
	// name in httpapi, never by a service token).
	return "", "", false
}

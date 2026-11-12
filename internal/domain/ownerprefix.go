package domain

import (
	"fmt"
	"strings"
)

// OwnerPrefix turns an owner (a user name or an email address) into the
// component that prefixes the branches that person makes on the user page.
// The rule is fixed so that the console and the server always agree:
//
//  1. an email address keeps only its local part (before the first "@");
//  2. letters are lowercased;
//  3. every run of characters other than a-z and 0-9 becomes one hyphen;
//  4. leading and trailing hyphens are dropped;
//  5. if the result is empty or starts with a digit, "u" is put in front
//     (names must start with a letter), so "42" becomes "u42" and "" "u".
//
// Different owners can end up with the same prefix (alice@a.com and
// alice@b.com); their equal names then collide as before.
func OwnerPrefix(owner string) string {
	local, _, _ := strings.Cut(owner, "@")
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	p := strings.TrimSuffix(b.String(), "-")
	if p == "" || (p[0] >= '0' && p[0] <= '9') {
		p = "u" + p
	}
	return p
}

// PrefixedName is the branch name for what owner typed on the user page:
// OwnerPrefix(owner) + "-" + typed. A name that already starts with that
// prefix and a hyphen is kept as it is. A name that would exceed
// MaxNameLength is refused (never truncated), telling how long the typed
// part may be.
func PrefixedName(owner, typed string) (string, error) {
	prefix := OwnerPrefix(owner) + "-"
	maxTyped := MaxNameLength - len(prefix)
	if maxTyped < 1 {
		return "", fmt.Errorf("%w: the prefix %q made from your user name is too long to leave room for a branch name (the most is %d characters in all)", ErrInvalidName, prefix, MaxNameLength)
	}
	rest, had := strings.CutPrefix(typed, prefix)
	if !had {
		rest = typed
	}
	if len(rest) > maxTyped {
		return "", fmt.Errorf("%w %q: branch names made here start with %q, so the part you type may have at most %d characters (%d in all)", ErrInvalidName, typed, prefix, maxTyped, MaxNameLength)
	}
	if _, err := ParseName(rest); err != nil {
		return "", err
	}
	return prefix + rest, nil
}

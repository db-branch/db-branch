// Package domain holds db-branch entities, value objects and ports.
// It must not depend on anything outside the standard library.
package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidName is returned when a branch or base name breaks the naming rules.
var ErrInvalidName = errors.New("invalid name")

// MaxNameLength keeps names usable in ZFS dataset names, DB user names and DNS labels.
const MaxNameLength = 32

var namePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9])*$`)

// Name identifies a branch or a base: lowercase letters, digits and single
// hyphens, starting with a letter. The restriction makes names safe to pass
// to zfs, SQL and ProxySQL without quoting.
type Name struct{ s string }

// ParseName validates s.
func ParseName(s string) (Name, error) {
	if len(s) > MaxNameLength || !namePattern.MatchString(s) {
		return Name{}, fmt.Errorf("%w %q: use 1-%d lowercase letters, digits or single hyphens, starting with a letter",
			ErrInvalidName, s, MaxNameLength)
	}
	return Name{s: s}, nil
}

func (n Name) String() string { return n.s }

// MarshalText stores a Name as its plain string.
func (n Name) MarshalText() ([]byte, error) { return []byte(n.s), nil }

// UnmarshalText validates the name while decoding. An empty string is the unset Name.
func (n *Name) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*n = Name{}
		return nil
	}
	parsed, err := ParseName(string(b))
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}

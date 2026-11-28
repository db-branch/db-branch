package domain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ProtocolUsers is the cluster protocol from which multi-user OSS accounts are
// replicated in Raft (insert_user, put_user, delete_user).
const ProtocolUsers = 30

// ProtocolPHCPassword is the cluster protocol from which stored password hashes
// use Argon2id PHC strings instead of raw 32-byte keys.
const ProtocolPHCPassword = 32

// AdminUserID is the immutable principal ID of the built-in admin account.
const AdminUserID = "admin"

// UserIDPrefix is the immutable principal ID prefix of email-based users.
const UserIDPrefix = "user:"

var emailPattern = regexp.MustCompile(`^[a-z0-9._%+-]+@([a-z0-9-]+\.)+[a-z]{2,}$`)

// User is a console user account stored in core state (either the built-in
// "admin" account or an email-identified account "user:<email>").
type User struct {
	ID         string    `json:"id"`
	Email      string    `json:"email,omitempty"`
	Name       string    `json:"name"`
	Salt       []byte    `json:"salt,omitempty"`
	Hash       []byte    `json:"hash,omitempty"`
	Disabled   bool      `json:"disabled,omitempty"`
	MustChange bool      `json:"must_change,omitempty"`
	SessionGen int       `json:"session_gen,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Clone returns a deep copy of u (copying Salt and Hash slices).
func (u User) Clone() User {
	u.Salt = append([]byte(nil), u.Salt...)
	u.Hash = append([]byte(nil), u.Hash...)
	return u
}

// ActiveWithPassword reports whether u can sign in with a password.
func (u User) ActiveWithPassword() bool {
	return !u.Disabled && len(u.Hash) > 0
}

// DisplayName returns u's display name, falling back to Email or ID.
func (u User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// UserStore persists console user accounts.
type UserStore interface {
	ListUsers(ctx context.Context) ([]User, error)
	GetUser(ctx context.Context, id string) (User, error)
	InsertUser(ctx context.Context, u User) error
	PutUser(ctx context.Context, u User) error
	DeleteUser(ctx context.Context, id string) error
}

// NormalizeEmail canonicalizes an email address.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail verifies that raw is a valid, normalized-compatible email address.
func ValidateEmail(raw string) error {
	email := NormalizeEmail(raw)
	if len(email) > 200 || !emailPattern.MatchString(email) {
		return fmt.Errorf("%w: %q is not a valid email address", ErrInvalidName, raw)
	}
	return nil
}

// UserIDForEmail returns the immutable user ID for email.
func UserIDForEmail(email string) string {
	return UserIDPrefix + NormalizeEmail(email)
}

// ResolveUserID normalizes an ID or email ("admin", "user:<email>", or "<email>")
// into a canonical user ID.
func ResolveUserID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.EqualFold(s, AdminUserID) {
		return AdminUserID, nil
	}
	if email, ok := strings.CutPrefix(s, UserIDPrefix); ok {
		if err := ValidateEmail(email); err != nil {
			return "", err
		}
		return UserIDForEmail(email), nil
	}
	if strings.EqualFold(strings.TrimSpace(s), "") {
		return "", fmt.Errorf("%w: user identifier is required", ErrInvalidName)
	}
	if err := ValidateEmail(s); err != nil {
		return "", err
	}
	return UserIDForEmail(s), nil
}

// CountActivePasswordUsers returns how many users in users are enabled and have
// a password hash.
func CountActivePasswordUsers(users map[string]User) int {
	n := 0
	for _, u := range users {
		if u.ActiveWithPassword() {
			n++
		}
	}
	return n
}

// EnsureActivePasswordUserAfterUpdate reports whether replacing users[u.ID] with
// u leaves at least one active password-authenticated user (counting legacyAdminActive
// only when users is still empty).
func EnsureActivePasswordUserAfterUpdate(users map[string]User, u User, legacyAdminActive bool) error {
	count := 0
	for id, cur := range users {
		if id == u.ID {
			continue
		}
		if cur.ActiveWithPassword() {
			count++
		}
	}
	if u.ActiveWithPassword() {
		count++
	}
	if len(users) == 0 && legacyAdminActive {
		count++
	}
	if count == 0 {
		return fmt.Errorf("%w: at least one active user with a password must remain", ErrConflict)
	}
	return nil
}

// EnsureActivePasswordUserAfterDelete reports whether deleting id leaves at
// least one active password-authenticated user.
func EnsureActivePasswordUserAfterDelete(users map[string]User, id string, legacyAdminActive bool) error {
	count := 0
	for curID, cur := range users {
		if curID == id {
			continue
		}
		if cur.ActiveWithPassword() {
			count++
		}
	}
	if len(users) == 0 && legacyAdminActive {
		count++
	}
	if count == 0 {
		return fmt.Errorf("%w: cannot delete the last active user", ErrConflict)
	}
	return nil
}

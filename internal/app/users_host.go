package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

type userHostAdapter struct {
	auth   *auth.Service
	tokens *token.Service
	log    *slog.Logger
}

func newUserHost(a *auth.Service, tokens *token.Service, log *slog.Logger) ext.UserStore {
	if a == nil || a.Users == nil {
		return nil
	}
	return userHostAdapter{auth: a, tokens: tokens, log: log}
}

func toExtUser(u domain.User) ext.User {
	return ext.User{
		ID:          u.ID,
		Email:       u.Email,
		Name:        u.DisplayName(),
		HasPassword: len(u.Hash) > 0,
		Disabled:    u.Disabled,
		MustChange:  u.MustChange,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

func mapUserErr(err error) error {
	if err == nil {
		return nil
	}
	var rl *auth.RateLimitedError
	switch {
	case errors.As(err, &rl):
		return &ext.RateLimitedError{RetryAfter: rl.RetryAfter}
	case errors.Is(err, auth.ErrInvalidCredentials):
		return fmt.Errorf("%w: %v", ext.ErrUnauthenticated, err)
	case errors.Is(err, auth.ErrWeakPassword):
		return fmt.Errorf("%w: %v", ext.ErrWeakPassword, err)
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%w: %v", ext.ErrNotFound, err)
	case errors.Is(err, domain.ErrAlreadyExists):
		return fmt.Errorf("%w: %v", ext.ErrAlreadyExists, err)
	case errors.Is(err, domain.ErrConflict):
		return fmt.Errorf("%w: %v", ext.ErrConflict, err)
	case errors.Is(err, domain.ErrClusterUpgrading):
		return fmt.Errorf("%w: %v", ext.ErrClusterUpgrading, err)
	case errors.Is(err, domain.ErrInvalidName):
		return fmt.Errorf("%w: %v", ext.ErrInvalid, err)
	default:
		return err
	}
}

func (h userHostAdapter) ListUsers(ctx context.Context) ([]ext.User, error) {
	users, err := h.auth.ListUsers(ctx)
	if err != nil {
		return nil, mapUserErr(err)
	}
	out := make([]ext.User, len(users))
	for i, u := range users {
		out[i] = toExtUser(u)
	}
	return out, nil
}

func (h userHostAdapter) GetUser(ctx context.Context, id string) (ext.User, error) {
	u, err := h.auth.GetUser(ctx, id)
	if err != nil {
		return ext.User{}, mapUserErr(err)
	}
	return toExtUser(u), nil
}

func (h userHostAdapter) CreateUser(ctx context.Context, in ext.UserCreate) (ext.User, error) {
	u, err := h.auth.CreateUser(ctx, auth.CreateUserInput{
		Email:         in.Email,
		Name:          in.Name,
		Password:      in.Password,
		EmptyPassword: in.EmptyPassword,
		MustChange:    in.MustChange,
		Disabled:      in.Disabled,
	})
	if err != nil {
		return ext.User{}, mapUserErr(err)
	}
	return toExtUser(u), nil
}

func (h userHostAdapter) UpdateUser(ctx context.Context, id string, in ext.UserUpdate) (ext.User, error) {
	u, err := h.auth.UpdateUser(ctx, id, auth.UpdateUserInput{
		Name:          in.Name,
		Password:      in.Password,
		ClearPassword: in.ClearPassword,
		Disabled:      in.Disabled,
		MustChange:    in.MustChange,
	})
	if err != nil {
		return ext.User{}, mapUserErr(err)
	}
	if in.Disabled != nil && *in.Disabled && h.auth.RevokeTokens == nil && h.tokens != nil {
		if rerr := h.tokens.RevokeByOwner(ctx, u.ID); rerr != nil && h.log != nil {
			h.log.Warn("revoking personal tokens for disabled user", "owner", u.ID, "err", rerr)
		}
	}
	return toExtUser(u), nil
}

func (h userHostAdapter) DeleteUser(ctx context.Context, id string) error {
	resolved, _ := domain.ResolveUserID(id)
	if err := h.auth.DeleteUser(ctx, id); err != nil {
		return mapUserErr(err)
	}
	if h.auth.RevokeTokens == nil && h.tokens != nil && resolved != "" {
		if rerr := h.tokens.RevokeByOwner(ctx, resolved); rerr != nil && h.log != nil {
			h.log.Warn("revoking personal tokens for deleted user", "owner", resolved, "err", rerr)
		}
	}
	return nil
}

func (h userHostAdapter) VerifyPassword(ctx context.Context, client, username, password string) (ext.User, error) {
	u, err := h.auth.VerifyUserFrom(client, username, password)
	if err != nil {
		return ext.User{}, mapUserErr(err)
	}
	return toExtUser(u), nil
}

// mergeSingleNodeUsers reconciles snapUsers (state.json) with auth.json creds
// so the newer built-in "admin" password/updated_at wins on cluster init.
func mergeSingleNodeUsers(snapUsers []domain.User, cred auth.Credentials, hasCred bool) []domain.User {
	byID := make(map[string]domain.User, len(snapUsers)+1)
	for _, u := range snapUsers {
		byID[u.ID] = u.Clone()
	}
	if hasCred && len(cred.Hash) > 0 {
		if adm, ok := byID[domain.AdminUserID]; !ok {
			byID[domain.AdminUserID] = domain.User{
				ID: domain.AdminUserID, Name: domain.AdminUserID,
				Salt: cred.Salt, Hash: cred.Hash,
				MustChange: cred.MustChange, SessionGen: max(cred.Generation, 1),
				CreatedAt: cred.UpdatedAt, UpdatedAt: cred.UpdatedAt,
			}
		} else if cred.UpdatedAt.After(adm.UpdatedAt) {
			adm.Salt = cred.Salt
			adm.Hash = cred.Hash
			adm.Disabled = false
			adm.MustChange = cred.MustChange
			if adm.SessionGen < cred.Generation {
				adm.SessionGen = cred.Generation
			}
			adm.UpdatedAt = cred.UpdatedAt
			byID[domain.AdminUserID] = adm
		}
	}
	out := make([]domain.User, 0, len(byID))
	for _, u := range byID {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// migrateAdminUserIfEmpty seeds built-in "admin" in UserStore when Users is
// empty and legacy credentials already exist (never recreating a deleted admin).
func migrateAdminUserIfEmpty(ctx context.Context, a *auth.Service, log *slog.Logger) {
	if a == nil || a.Users == nil || a.Store == nil {
		return
	}
	if a.Protocol != nil && a.Protocol() < domain.ProtocolUsers {
		return
	}
	users, err := a.Users.ListUsers(ctx)
	if err != nil || len(users) > 0 {
		return
	}
	c, err := a.Store.Load()
	if err != nil || len(c.Hash) == 0 {
		return
	}
	admin := domain.User{
		ID: domain.AdminUserID, Name: domain.AdminUserID,
		Salt: c.Salt, Hash: c.Hash,
		MustChange: c.MustChange, SessionGen: max(c.Generation, 1),
		CreatedAt: c.UpdatedAt, UpdatedAt: c.UpdatedAt,
	}
	if err := a.Users.InsertUser(ctx, admin); err != nil && !errors.Is(err, domain.ErrAlreadyExists) && !errors.Is(err, domain.ErrClusterUpgrading) && log != nil {
		log.Warn("migrating built-in admin to user store", "err", err)
	}
}

func adoptClusterAdminUser(ctx context.Context, store *raftstore.Store, cur auth.Credentials) {
	if store.ClusterProtocol() < domain.ProtocolUsers || len(cur.Hash) == 0 {
		return
	}
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) > 0 {
		return
	}
	_ = store.InsertUser(ctx, domain.User{
		ID: domain.AdminUserID, Name: domain.AdminUserID,
		Salt: cur.Salt, Hash: cur.Hash,
		MustChange: cur.MustChange, SessionGen: max(cur.Generation, 1),
		CreatedAt: cur.UpdatedAt, UpdatedAt: cur.UpdatedAt,
	})
}

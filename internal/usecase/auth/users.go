package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// CreateUserInput specifies a new console user account.
type CreateUserInput struct {
	Email         string
	Name          string
	Password      string
	EmptyPassword bool
	MustChange    bool
	Disabled      bool
}

// UpdateUserInput specifies fields to update on a console user account.
type UpdateUserInput struct {
	Name          *string
	Password      *string
	ClearPassword bool
	Disabled      *bool
	MustChange    *bool
}

func (s *Service) validatePayloadUser(c Credentials, p payload) (mustChange, valid bool, err error) {
	if s.Users == nil {
		if p.Gen != c.Generation {
			return false, false, nil
		}
		return c.MustChange && p.User == AdminUser, true, nil
	}
	if p.UGen != nil {
		u, uerr := s.Users.GetUser(context.Background(), p.User)
		if errors.Is(uerr, domain.ErrNotFound) {
			return false, false, nil
		}
		if uerr != nil {
			return false, false, uerr
		}
		if u.Disabled || *p.UGen != u.SessionGen {
			return false, false, nil
		}
		return u.MustChange, true, nil
	}
	// Legacy cookie (UGen == nil) or external/unmigrated user.
	if p.Gen != c.Generation {
		return false, false, nil
	}
	u, uerr := s.Users.GetUser(context.Background(), p.User)
	if uerr == nil {
		if u.Disabled || u.SessionGen > c.Generation {
			return false, false, nil
		}
		return u.MustChange, true, nil
	}
	if errors.Is(uerr, domain.ErrNotFound) {
		if p.User == AdminUser {
			if s.Protocol != nil && s.Protocol() < domain.ProtocolUsers {
				return c.MustChange, true, nil
			}
			return false, false, nil
		}
		return false, true, nil
	}
	return false, false, uerr
}

func (s *Service) setAdminUserPassword(c Credentials, loadErr error, salt, hash, key []byte, now time.Time, mustChange bool) error {
	ctx := context.Background()
	u, uerr := s.Users.GetUser(ctx, AdminUser)
	if uerr != nil && !errors.Is(uerr, domain.ErrNotFound) {
		return uerr
	}
	if uerr == nil && s.Protocol != nil && s.Protocol() < domain.ProtocolUsers {
		return domain.ErrClusterUpgrading
	}
	if errors.Is(loadErr, ErrNoCredentials) {
		c = Credentials{Salt: salt, Hash: hash, Generation: 1, UpdatedAt: now, SessionKey: key, MustChange: mustChange}
		if err := s.Store.Save(c); err != nil {
			return err
		}
	} else if len(c.SessionKey) == 0 {
		c.SessionKey = key
		if err := s.Store.Save(c); err != nil {
			return err
		}
	}
	mirrorCreds := func() error {
		gen := c.Generation
		if gen < 1 {
			gen = 1
		}
		return s.Store.Save(Credentials{
			Salt:       salt,
			Hash:       hash,
			Generation: gen,
			UpdatedAt:  now,
			SessionKey: key,
			MustChange: mustChange,
		})
	}
	if uerr == nil {
		applyAdminPassword(&u, c.Generation, salt, hash, now, mustChange)
		if err := s.Users.PutUser(ctx, u); err != nil {
			return err
		}
		return mirrorCreds()
	}
	gen := 1
	if loadErr == nil {
		gen = c.Generation + 1
	}
	admin := domain.User{
		ID: AdminUser, Name: AdminUser,
		Salt: salt, Hash: hash,
		MustChange: mustChange, SessionGen: gen,
		CreatedAt: now, UpdatedAt: now,
	}
	err := s.Users.InsertUser(ctx, admin)
	if errors.Is(err, domain.ErrClusterUpgrading) && loadErr == nil {
		return s.Store.Save(Credentials{
			Salt: salt, Hash: hash, Generation: c.Generation + 1, UpdatedAt: now, SessionKey: key, MustChange: mustChange,
		})
	}
	if errors.Is(err, domain.ErrAlreadyExists) {
		existing, gerr := s.Users.GetUser(ctx, AdminUser)
		if gerr != nil {
			return gerr
		}
		applyAdminPassword(&existing, c.Generation, salt, hash, now, mustChange)
		if err := s.Users.PutUser(ctx, existing); err != nil {
			return err
		}
		return mirrorCreds()
	}
	if err != nil {
		return err
	}
	return mirrorCreds()
}

func applyAdminPassword(u *domain.User, credGen int, salt, hash []byte, now time.Time, mustChange bool) {
	if u.Name == "" {
		u.Name = AdminUser
	}
	if u.SessionGen < credGen {
		u.SessionGen = credGen
	}
	u.SessionGen++
	u.Salt = salt
	u.Hash = hash
	u.Disabled = false
	u.MustChange = mustChange
	u.UpdatedAt = now
}

func (s *Service) hashNewPassword(password string) (salt, hash []byte, err error) {
	if len(password) < minPasswordLen || len(password) > MaxPasswordLen {
		return nil, nil, ErrWeakPassword
	}
	salt = make([]byte, 16)
	_, _ = rand.Read(salt)
	return salt, s.Hasher.Hash(password, salt), nil
}

// ListUsers lists all core console users sorted by ID.
func (s *Service) ListUsers(ctx context.Context) ([]domain.User, error) {
	return s.Users.ListUsers(ctx)
}

// GetUser looks up a user by ID ("admin" or "user:<email>") or email.
func (s *Service) GetUser(ctx context.Context, idOrEmail string) (domain.User, error) {
	id, err := domain.ResolveUserID(idOrEmail)
	if err != nil {
		return domain.User{}, err
	}
	return s.Users.GetUser(ctx, id)
}

// CreateUser creates a new email-identified console user.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (domain.User, error) {
	if err := domain.ValidateEmail(in.Email); err != nil {
		return domain.User{}, err
	}
	email := domain.NormalizeEmail(in.Email)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = email
	}
	var salt, hash []byte
	if !in.EmptyPassword || in.Password != "" {
		var err error
		salt, hash, err = s.hashNewPassword(in.Password)
		if err != nil {
			return domain.User{}, err
		}
	}
	now := s.Clock().UTC()
	u := domain.User{
		ID:         domain.UserIDForEmail(email),
		Email:      email,
		Name:       name,
		Salt:       salt,
		Hash:       hash,
		Disabled:   in.Disabled,
		MustChange: in.MustChange && len(hash) > 0,
		SessionGen: 1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.Users.InsertUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	return u, nil
}

// UpdateUser updates an existing console user.
func (s *Service) UpdateUser(ctx context.Context, idOrEmail string, in UpdateUserInput) (domain.User, error) {
	id, err := domain.ResolveUserID(idOrEmail)
	if err != nil {
		return domain.User{}, err
	}
	if s.Protocol != nil && s.Protocol() < domain.ProtocolUsers {
		return domain.User{}, domain.ErrClusterUpgrading
	}
	u, err := s.Users.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	credGen := 0
	if id == AdminUser {
		if c, cerr := s.Store.Load(); cerr == nil {
			credGen = c.Generation
		}
	}
	bumpGen := false
	if in.Name != nil {
		u.Name = strings.TrimSpace(*in.Name)
	}
	if in.ClearPassword {
		u.Salt = nil
		u.Hash = nil
		u.MustChange = false
		bumpGen = true
	} else if in.Password != nil {
		salt, hash, herr := s.hashNewPassword(*in.Password)
		if herr != nil {
			return domain.User{}, herr
		}
		u.Salt = salt
		u.Hash = hash
		if in.MustChange == nil {
			u.MustChange = false
		}
		bumpGen = true
	}
	if in.Disabled != nil {
		if *in.Disabled && !u.Disabled {
			bumpGen = true
		}
		u.Disabled = *in.Disabled
	}
	if in.MustChange != nil {
		u.MustChange = *in.MustChange && len(u.Hash) > 0
	}
	if bumpGen {
		if u.SessionGen < credGen {
			u.SessionGen = credGen
		}
		u.SessionGen++
	}
	u.UpdatedAt = s.Clock().UTC()
	if err := s.Users.PutUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	if in.Disabled != nil && *in.Disabled && s.RevokeTokens != nil {
		if err := s.RevokeTokens(ctx, u.ID); err != nil {
			return domain.User{}, err
		}
	}
	return u, nil
}

// DeleteUser removes a user (preserving at least one active password user).
func (s *Service) DeleteUser(ctx context.Context, idOrEmail string) error {
	id, err := domain.ResolveUserID(idOrEmail)
	if err != nil {
		return err
	}
	if err := s.Users.DeleteUser(ctx, id); err != nil {
		return err
	}
	if s.RevokeTokens != nil {
		return s.RevokeTokens(ctx, id)
	}
	return nil
}

// ResetUserPassword sets a new random 24-character password on idOrEmail.
func (s *Service) ResetUserPassword(ctx context.Context, idOrEmail string, mustChange bool) (domain.User, string, error) {
	pw := GeneratePassword()
	u, err := s.UpdateUser(ctx, idOrEmail, UpdateUserInput{Password: &pw, MustChange: &mustChange})
	if err != nil {
		return domain.User{}, "", err
	}
	return u, pw, nil
}

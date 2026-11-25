package domain

import (
	"context"
	"time"
)

// Token scopes.
const (
	// ScopeFull may do everything an admin can.
	ScopeFull = "full"
	// ScopeBranches may create, use and remove branches and read bases:
	// what a developer or an LLM agent needs, nothing that changes the cluster.
	ScopeBranches = "branches"
)

// APIToken is a bearer token for API clients (and MCP). Only a hash of the
// secret is kept.
type APIToken struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Owner is the principal a personal token acts as (empty: a service
	// token, acting as itself); OwnerName is its display name; OwnerRoles keeps
	// the owner's non-admin roles at issuance so role-based authorizers enforce
	// the owner's limits on the token.
	Owner      string   `json:"owner,omitempty"`
	OwnerName  string   `json:"owner_name,omitempty"`
	OwnerRoles []string `json:"owner_roles,omitempty"`
	// Bases and Team optionally restrict a service token with ScopeBranches
	// to specific bases and/or a linked team's limits and branch sharing.
	Bases []string `json:"bases,omitempty"`
	Team  string   `json:"team,omitempty"`
}

// ProtocolPersonalTokenRoles is the cluster protocol from which personal API
// tokens persist OwnerRoles so older nodes cannot drop them during upgrade.
const ProtocolPersonalTokenRoles = 28

// ProtocolServiceTokenRestrictions is the cluster protocol from which service
// tokens persist Bases and Team restrictions so older nodes cannot drop them.
const ProtocolServiceTokenRestrictions = 29

// TokenStore keeps API tokens.
type TokenStore interface {
	ListTokens(ctx context.Context) ([]APIToken, error)
	PutToken(ctx context.Context, t APIToken) error
	DeleteToken(ctx context.Context, id string) error
}

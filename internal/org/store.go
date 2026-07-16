package org

import (
	"context"
	"errors"
)

// ErrNotFound is returned by a Store when no matching org/member/invitation row
// exists. Callers use errors.Is to distinguish "not found" from unexpected DB
// errors.
var ErrNotFound = errors.New("org: not found")

// Store is the data-access contract for the multi-tenant identity model
// (orgs / org_members / org_invitations). Users are NOT owned here — they live
// in auth.UserStore; membership references users by email.
//
// Two implementations must stay byte-for-byte equivalent in semantics
// (SQLStore persistent, MemStore in-memory): email normalized to lower-case,
// defaults filled, AddOrgMember is an upsert, ErrNotFound on miss, list order
// newest→oldest, returned values are copies. Any drift between the two is a
// silent multi-tenant authz hazard.
type Store interface {
	// ── orgs ──────────────────────────────────────────────────────────────
	CreateOrg(ctx context.Context, o *Org) error
	GetOrg(ctx context.Context, id string) (*Org, error)
	GetOrgBySlug(ctx context.Context, slug string) (*Org, error)
	ListOrgs(ctx context.Context) ([]*Org, error)
	UpdateOrg(ctx context.Context, o *Org) error
	DeleteOrg(ctx context.Context, id string) error
	// ListOrgsForEmail returns every org the given member email belongs to
	// (newest→oldest), with each Org.Role populated to the caller's role.
	ListOrgsForEmail(ctx context.Context, email string) ([]*Org, error)
	SetOrgStatus(ctx context.Context, id, status string) error
	CountOrgAdmins(ctx context.Context, orgID string) (int, error)
	// CountOrgOwners counts owner-role members; the API layer uses it to block
	// removing/demoting the last owner.
	CountOrgOwners(ctx context.Context, orgID string) (int, error)
	// CountOrgs returns the total number of org rows; used by the bootstrap to
	// decide whether the default org needs to be seeded.
	CountOrgs(ctx context.Context) (int, error)

	// ── members ───────────────────────────────────────────────────────────
	// AddOrgMember upserts a (org_id, email) membership (idempotent; updates
	// role on conflict, preserving the original row id).
	AddOrgMember(ctx context.Context, m *Member) error
	GetOrgMember(ctx context.Context, orgID, email string) (*Member, error)
	ListOrgMembers(ctx context.Context, orgID string) ([]*Member, error)
	RemoveOrgMember(ctx context.Context, orgID, email string) error

	// ── invitations ───────────────────────────────────────────────────────
	CreateInvitation(ctx context.Context, inv *Invitation) error
	GetInvitationByToken(ctx context.Context, token string) (*Invitation, error)
	ListInvitations(ctx context.Context, orgID string) ([]*Invitation, error)
	SetInvitationStatus(ctx context.Context, id, status string) error
}

// Compile-time assertions that both implementations satisfy the interface.
var (
	_ Store = (*SQLStore)(nil)
	_ Store = (*MemStore)(nil)
)

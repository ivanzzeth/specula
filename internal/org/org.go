// Package org is the multi-tenant identity/organization model: orgs,
// org_members (keyed by email), and org_invitations. Ported/adapted from
// ai-sandbox internal/controlplane/org.
//
// # Alignment with internal/auth
//
// Specula already owns the user account model in internal/auth (auth.User with
// an int64 ID, backed by auth.UserStore). This package does NOT redefine a User
// type nor manage a users table — it reuses auth.User. Membership is by email
// (org_members(org_id,email)), which matches auth.User.Email, so the two models
// join on email without a foreign key. The first-user-admin bootstrap wires the
// two together: auth.Service.Register creates the account, and this Store
// creates the DefaultOrg + an owner Member for that account's email.
//
// # ACL subject identity
//
// acl.Subject.UserID and Resource.OwnerUserID are strings. For a human user the
// canonical subject string is UserSubjectID(user.ID) ("user:<id>"); for an API
// key it is apikey.SubjectID(keyID) ("apikey:<id>"). The two namespaces never
// collide. repos.owner_user_id and resource_grants.subject_id store these
// strings verbatim.
package org

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Bootstrap identifiers. The guardrail is "empty" rather than a fixed id
// (deleting the default org must not resurrect it — see bootstrap).
const (
	// DefaultOrgID is the organization the first registered user owns and into
	// which any legacy/un-scoped data is backfilled. apikey.DefaultOrgID MUST
	// equal this value (enforced by construction in the apikey package).
	DefaultOrgID = "org_default"
	// DefaultOrgName / DefaultOrgSlug are the default organization's display
	// name and slug.
	DefaultOrgName = "Default"
	DefaultOrgSlug = "default"
)

// org role ladder. Ordering is viewer < editor < admin < owner (owner is the
// top). The system role axis (auth.User.SystemRole) reuses the first three
// (viewer<editor<admin) and has no owner — owner is purely the org-ownership
// dimension (billing / ownership-transfer / delete-org).
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
	// RoleOwner is the highest org role = admin + billing + ownership transfer +
	// delete org. The org creator becomes owner automatically; the last owner
	// cannot be removed or demoted (guardrail enforced at the API layer).
	RoleOwner = "owner"
)

// Organization lifecycle status (frozen = all access to the org is sealed).
const (
	StatusActive = "active"
	StatusFrozen = "frozen"
)

// roleRank maps an org role to its ladder position; unknown/empty ranks lowest.
func roleRank(role string) int {
	switch role {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// NormalizeRole maps an unknown/empty role to the most conservative viewer.
func NormalizeRole(role string) string {
	if r := NormalizeLegacyRole(role); r != "" {
		return r
	}
	return RoleViewer
}

// NormalizeLegacyRole maps a role value onto the four-rung ladder, translating
// the legacy aliases carried by historical rows (org_admin→admin, member→editor).
// It returns "" for an unrecognised value, letting callers distinguish "not a
// role I know" from a real role — NormalizeRole collapses that to viewer, which
// is right for storage but wrong for validating user input, where a typo should
// be rejected rather than silently downgraded.
func NormalizeLegacyRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleOwner:
		return RoleOwner
	case RoleAdmin, "org_admin":
		return RoleAdmin
	case RoleEditor, "member":
		return RoleEditor
	case RoleViewer:
		return RoleViewer
	}
	return ""
}

// NormalizeSystemRole maps a system-role value (auth.User.SystemRole) onto the
// backoffice ladder, which reuses viewer<editor<admin and has no owner. Both ""
// and the legacy "user" mean NO system access and normalise to "" — the
// distinction is load-bearing: a system role grants implicit read-only sight of
// every org, so treating the "user" every ordinary account carries as a system
// role would hand all of them cross-tenant read.
func NormalizeSystemRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleViewer, RoleEditor, RoleAdmin:
		return strings.ToLower(strings.TrimSpace(role))
	}
	return ""
}

// AtLeast reports whether have satisfies the want role threshold on the ladder
// (viewer<editor<admin<owner). Use for "needs org role >= editor" checks.
func AtLeast(have, want string) bool {
	return roleRank(have) >= roleRank(want)
}

// UserSubjectID returns the canonical acl.Subject.UserID / Resource.OwnerUserID
// string for a human user identified by its auth.User.ID. Namespaced with
// "user:" so it never collides with an API-key synthetic subject ("apikey:<id>").
func UserSubjectID(userID int64) string {
	return "user:" + strconv.FormatInt(userID, 10)
}

// Org is an organization (workspace). All org-level resources belong to it via
// OrgID.
type Org struct {
	ID        string
	Name      string
	Slug      string
	Status    string // active | frozen
	CreatedBy string // acl subject string of the creator
	CreatedAt time.Time

	// Role and SystemAccess are per-request transient fields (not persisted,
	// not scanned/inserted by CRUD), populated by the auth layer:
	//   Role         the caller's effective role in this org (viewer|editor|
	//                admin|owner); an API key is always admin within its org.
	//   SystemAccess whether this access comes from an implicit cross-org
	//                read-only system role (read-only; does not reach member/key
	//                management).
	Role         string
	SystemAccess bool
}

// Frozen reports whether the organization is frozen (all access sealed).
func (o *Org) Frozen() bool { return o != nil && o.Status == StatusFrozen }

// Member is a single (org, email) membership record. role is the org role
// (viewer|editor|admin|owner). The org creator is owner; members added via the
// invitation endpoint default to the least privilege (viewer). InvitedBy records
// who brought the member in (backfilled from org_invitations.invited_by on
// accept; empty for a direct add).
type Member struct {
	ID        string
	OrgID     string
	Email     string
	Role      string
	InvitedBy string
	CreatedAt time.Time
}

// Invitation status ladder (org_invitations.status). Only "accepted" writes an
// org_members row.
const (
	InviteStatusPending  = "pending"
	InviteStatusAccepted = "accepted"
	InviteStatusDeclined = "declined"
	InviteStatusExpired  = "expired"
)

// Invitation is a pending membership invitation. token is high-entropy and
// unique; accept/decline locate by token. expires_at gates lazy expiry.
// Creating an invitation does NOT create a member — only the invitee accepting
// writes an org_members row (with invited_by backfilled).
type Invitation struct {
	ID        string
	OrgID     string
	Email     string
	Role      string
	InvitedBy string
	Token     string
	Status    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Expired reports whether the invitation has expired (ExpiresAt non-zero and in
// the past).
func (i *Invitation) Expired() bool {
	return i != nil && !i.ExpiresAt.IsZero() && time.Now().After(i.ExpiresAt)
}

// newID generates a prefixed stable identifier (e.g. "org_", "mem_", "inv_")
// backed by crypto/rand.
func newID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

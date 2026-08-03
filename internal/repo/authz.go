package repo

import (
	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/org"
)

// OrgRoleGrant is the org-RBAC axis of a hosted-repo access decision: the role
// the caller actually holds in the repo's org, and the rung that substitutes for
// per-resource ownership.
//
// It is a struct rather than two positional strings on purpose: swapping "the
// role you have" with "the role you need" would silently grant everyone access,
// and a positional (string, string) pair makes that a one-character mistake.
//
// Have MUST be the caller's effective role in the RESOURCE's org — never a role
// from some other org they belong to. "" means "no role there". OrgID records
// which org Have was resolved against so Authorize can refuse a role that came
// from the wrong tenant (a belt against the cross-tenant bleed that follows from
// passing "some org the caller is an admin of" instead of "this repo's org").
type OrgRoleGrant struct {
	OrgID string // org Have was resolved in; must equal the resource's OrgID
	Have  string // caller's effective role in that org ("" = none)
	Need  string // rung that substitutes for ownership (e.g. org.RoleEditor)
}

// allows reports whether the held role satisfies the required rung for a
// resource in resourceOrgID.
//
// NormalizeLegacyRole (not NormalizeRole) is deliberate: NormalizeRole maps an
// unrecognised value to viewer, which would turn a typo'd or corrupted role
// string into read access on every private repo in the org. An unknown role must
// mean "no role".
func (g OrgRoleGrant) allows(resourceOrgID string) bool {
	if g.OrgID == "" || g.OrgID != resourceOrgID {
		return false // no org, or a role from a different tenant
	}
	have := org.NormalizeLegacyRole(g.Have)
	if have == "" || g.Need == "" {
		return false
	}
	return org.AtLeast(have, g.Need)
}

// Authorize is the single authorization decision for a hosted repo, shared by
// every surface that answers "may this caller pull/push/administer this repo" —
// the registry /token scope authorizer and the Admin API alike. Both MUST route
// through here: when they each rolled their own answer they disagreed about the
// very same repo (a key that could PATCH a repo through the Admin API got
// `access: null` from /token for a pull on it).
//
// Two deliberate axes, in order:
//
//  1. acl.CanAccessGranted — the authoritative per-resource decision: system
//     admin, the resource's owner, public read, and cross-org resource_grants.
//
//  2. The org role ladder — a repo belongs to an ORG, and org membership + role
//     is the real tenancy boundary; repos.owner_user_id is attribution. acl
//     models a repo as private|public with a single owner and cannot express the
//     ladder, so it is applied here. Without this axis the credential that
//     happened to push first became the repo's permanent sole owner: the org's
//     own admin key could not pull it, and rotating the push key orphaned the
//     repo with no recovery path.
//
// The role axis never widens access for a caller with no identity: an anonymous
// subject is refused before the ladder is consulted, so a role can only ever be
// an ADDITION to an authenticated caller's per-resource grant. It also cannot
// reach across tenants — the caller of this function resolves Have against the
// resource's own org, so a key pinned elsewhere arrives with Have == "".
//
// res is the resource profile: Repo.ToACLResource() for an existing repo, or the
// synthetic org-writable profile the registry uses for a not-yet-created repo on
// a first push. Returns nil to allow; acl.ErrReadOnly / acl.ErrForbidden to deny.
func Authorize(
	res acl.Resource,
	s acl.Subject,
	needWrite bool,
	grantedOrgs []string,
	role OrgRoleGrant,
) error {
	err := acl.CanAccessGranted(res, s, needWrite, grantedOrgs)
	if err == nil {
		return nil
	}
	if s.UserID == "" {
		return err // anonymous holds no role; never let one be claimed for it
	}
	if role.allows(res.OrgID) {
		return nil
	}
	return err
}

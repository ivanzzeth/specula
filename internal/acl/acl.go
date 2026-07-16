// Package acl is the single authoritative resource-authorization chokepoint for
// the whole platform. Ported as-is from ai-sandbox internal/controlplane/acl.
//
// Hard rule: any resource (repo / cache entry / org-scoped object) "who can
// read / who can write" decision MUST go through CanAccess or CanAccessGranted.
// Handlers must NOT inline their own visibility/owner/org checks — each resource
// type only maps itself onto a Resource (and the caller onto a Subject); the
// decision logic lives here and nowhere else.
//
// Four visibility levels:
//   - private: only the creator (owner); an empty owner = fail-closed (nobody
//     but a system admin).
//   - org + read:  same-org members may read.
//   - org + write: same-org members may read and write.
//   - public: anyone (including anonymous) may read; writes still require
//     owner / org-write / admin.
//
// The package is pure logic with no database dependency.
package acl

import "errors"

// Visibility controls the visible scope of a resource.
type Visibility string

const (
	Private Visibility = "private"
	Org     Visibility = "org"
	Public  Visibility = "public"
)

// Access is the in-org permission level used when Visibility == Org.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

var (
	// ErrForbidden: the caller has no access whatsoever to the resource.
	ErrForbidden = errors.New("acl: forbidden")
	// ErrReadOnly: the caller may read but not write (e.g. an org+read member
	// attempting a write, or an anonymous caller writing to a public resource).
	ErrReadOnly = errors.New("acl: read-only access")
)

// Subject is the principal making the access request. An empty UserID means an
// unauthenticated anonymous caller (can only ever match public read). API-key
// callers MUST use a stable synthetic UserID (e.g. "apikey:<keyID>") and never
// leave it empty.
type Subject struct {
	UserID string
	OrgID  string
	Admin  bool // system administrator: cross-org bypass (platform ops paths only)
}

// Resource is the authorization profile of the accessed resource. Each resource
// type provides a →Resource adapter to reuse this decision.
type Resource struct {
	OwnerUserID string
	OrgID       string
	Visibility  Visibility
	Access      Access
}

// NormalizeVisibility maps unknown/empty values to the most conservative private.
func NormalizeVisibility(v Visibility) Visibility {
	switch v {
	case Org, Public:
		return v
	default:
		return Private
	}
}

// NormalizeAccess maps unknown/empty values to the most conservative read.
func NormalizeAccess(a Access) Access {
	if a == Write {
		return Write
	}
	return Read
}

// CanAccess is the sole authoritative decision. needWrite=true covers
// attach/mutate/delete and other write operations. Returns nil to allow;
// ErrReadOnly / ErrForbidden to deny (the two are distinguished so callers can
// return a more accurate error / status code).
func CanAccess(r Resource, s Subject, needWrite bool) error {
	return CanAccessGranted(r, s, needWrite, nil)
}

// CanAccessGranted is the grant-aware authoritative decision: semantics are
// identical to CanAccess, the only difference being that orgs listed in
// grantedOrgs are treated as the same org as the resource, thereby obtaining
// org/read or org/write per the resource's Visibility/Access (e.g. a repo can be
// shared cross-org: feed the grant.GrantedOrgs result in here).
//   - private is still owner-only (grants do not relax private);
//   - public is unaffected (already readable by all; writes still require
//     owner / org-write / admin);
//   - grantedOrgs=nil is bit-for-bit equivalent to CanAccess.
func CanAccessGranted(r Resource, s Subject, needWrite bool, grantedOrgs []string) error {
	if needWrite {
		if canWrite(r, s, grantedOrgs) {
			return nil
		}
		if canRead(r, s, grantedOrgs) {
			return ErrReadOnly
		}
		return ErrForbidden
	}
	if canRead(r, s, grantedOrgs) {
		return nil
	}
	return ErrForbidden
}

func canRead(r Resource, s Subject, grantedOrgs []string) bool {
	if s.Admin {
		return true // system admin: cross-org bypass
	}
	if NormalizeVisibility(r.Visibility) == Public {
		return true // public: anyone (including anonymous) may read
	}
	if s.UserID == "" {
		return false // non-public requires authentication
	}
	if isOwner(r, s) {
		return true
	}
	// Same org (including grant-extended orgs) and the resource is open to the
	// org (org / public are both readable by same-org members).
	return sameOrg(r, s, grantedOrgs) && NormalizeVisibility(r.Visibility) != Private
}

func canWrite(r Resource, s Subject, grantedOrgs []string) bool {
	if s.UserID == "" {
		return false // anonymous never writes (public is read-only too)
	}
	if s.Admin {
		return true
	}
	if isOwner(r, s) {
		return true
	}
	// Same org (including grant-extended orgs) and an org+write member may
	// write. private/public do not grant write to non-owners.
	return sameOrg(r, s, grantedOrgs) &&
		NormalizeVisibility(r.Visibility) == Org &&
		NormalizeAccess(r.Access) == Write
}

// sameOrg reports whether subject and resource belong to the same org: either
// the resource org matches, or the subject org is present in grantedOrgs
// (cross-org grant). A grant hit requires a non-empty subject org to avoid an
// "empty org matches empty grant" false allow.
func sameOrg(r Resource, s Subject, grantedOrgs []string) bool {
	if r.OrgID == s.OrgID {
		return true
	}
	if s.OrgID == "" {
		return false
	}
	for _, g := range grantedOrgs {
		if g == s.OrgID {
			return true
		}
	}
	return false
}

// isOwner is a strict equality; an empty owner is always false (fail-closed: an
// API key with no synthetic subject and other legacy gaps must never be treated
// as "visible to everyone").
func isOwner(r Resource, s Subject) bool {
	return r.OwnerUserID != "" && r.OwnerUserID == s.UserID
}

// Package repo is the hosted-registry metadata model: org-owned repositories
// (the repos table, migration 0002) and their tag→digest pointers (the
// repo_tags table, migration 0003). It is the authoritative record that makes a
// pushed image "hosted" — as opposed to an upstream pull-through cache entry —
// and it carries the per-repo visibility (private|public) the registry authz
// layer feeds into acl.CanAccess.
//
// # Relationship to the cache tiers
//
// A hosted repo's blobs and manifests still live in the shared content-addressed
// store (CAS): push writes them there exactly like a pull-cache write, so a blob
// with the same digest is stored once regardless of origin. What this package
// adds on top is the mutable naming layer (which org owns "<org>/<repo>", what
// its visibility is, and which digest each tag currently points at) plus the
// "hosted, never GC-evicted" marker: a tag row here pins its manifest as
// authoritative data, not evictable cache.
//
// # Subject identity
//
// Repo.OwnerUserID stores an acl subject string — org.UserSubjectID(user.ID)
// for a human ("user:<id>") or apikey.SubjectID(keyID) ("apikey:<id>") — so it
// slots directly into acl.Resource.OwnerUserID. The org that owns a repo is the
// first path segment of "<org>/<repo>" (the registry namespace ↔ org binding).
package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ivanzzeth/specula/internal/acl"
)

// ErrNotFound is returned when no matching repo or tag row exists. Callers use
// errors.Is to distinguish "not found" from an unexpected database error.
var ErrNotFound = errors.New("repo: not found")

// Visibility values for a hosted repo. These are the string form persisted in
// repos.visibility; NormalizeVisibility maps them onto acl.Visibility for the
// authorization decision. Default (and fail-closed) is Private.
const (
	// VisibilityPrivate — only org members (per acl) may pull; push always
	// requires org write / owner / admin.
	VisibilityPrivate = "private"
	// VisibilityPublic — anyone, including anonymous, may pull; push still
	// requires org write / owner / admin.
	VisibilityPublic = "public"
)

// Repo is one row of the repos table: an org-owned hosted repository.
type Repo struct {
	ID          string    // stable identifier (repo_… )
	OrgID       string    // owning org (the "<org>" namespace segment)
	Name        string    // full repository name, canonically "<org>/<repo>"
	Visibility  string    // private | public (default private)
	OwnerUserID string    // acl subject string of the creator (user:… / apikey:…)
	CreatedAt   time.Time // first-push / creation time
}

// Tag is one row of the repo_tags table: a mutable tag→digest pointer for a
// hosted repo. Digest is the CAS key of the tagged manifest ("sha256:…").
type Tag struct {
	RepoID    string    // owning repo (repos.id)
	Tag       string    // human tag (e.g. "v1", "latest")
	Digest    string    // manifest digest the tag currently resolves to
	UpdatedAt time.Time // last push time for this tag
}

// ToACLResource maps a hosted repo onto the authorization profile acl.CanAccess
// consumes. Private/public map onto acl private/public; a repo is never modelled
// as acl "org+read/write" here because push/pull permission is decided by org
// membership + visibility, not a per-repo access column (cross-org sharing goes
// through the grant layer, which callers pass to acl.CanAccessGranted).
func (r *Repo) ToACLResource() acl.Resource {
	vis := acl.Private
	if NormalizeVisibility(r.Visibility) == VisibilityPublic {
		vis = acl.Public
	}
	return acl.Resource{
		OwnerUserID: r.OwnerUserID,
		OrgID:       r.OrgID,
		Visibility:  vis,
		Access:      acl.Read,
	}
}

// NormalizeVisibility maps an unknown/empty visibility to the most conservative
// private.
func NormalizeVisibility(v string) string {
	if v == VisibilityPublic {
		return VisibilityPublic
	}
	return VisibilityPrivate
}

// RepoStore is the persistence contract for hosted repositories.
type RepoStore interface {
	// CreateRepo inserts a hosted repo owned by orgID. visibility defaults to
	// private when empty; ownerUserID is the creator's acl subject string. The
	// returned Repo has ID / CreatedAt populated. Implementations must reject a
	// duplicate (org_id, name) — the repos_org_name unique index backs this.
	CreateRepo(ctx context.Context, orgID, name, visibility, ownerUserID string) (*Repo, error)
	// GetRepo returns the repo for (orgID, name), or ErrNotFound.
	GetRepo(ctx context.Context, orgID, name string) (*Repo, error)
	// ListRepos returns all repos in an org, newest-first.
	ListRepos(ctx context.Context, orgID string) ([]*Repo, error)
	// SetVisibility updates a repo's visibility (private|public).
	SetVisibility(ctx context.Context, orgID, name, visibility string) error
	// DeleteRepo removes the repo and (implementation choice) its tag rows.
	DeleteRepo(ctx context.Context, orgID, name string) error
}

// TagStore is the persistence contract for a hosted repo's tag→digest pointers.
// Tags are keyed by the owning repo's ID (repos.id), so tag names are scoped per
// repo and never collide across repos.
type TagStore interface {
	// PutTag upserts the tag→digest pointer for repoID (idempotent; overwrites
	// the digest and bumps UpdatedAt on conflict).
	PutTag(ctx context.Context, repoID, tag, digest string) error
	// GetTag returns the pointer for (repoID, tag), or ErrNotFound.
	GetTag(ctx context.Context, repoID, tag string) (*Tag, error)
	// ListTags returns all tags for a repo, tag-name ascending.
	ListTags(ctx context.Context, repoID string) ([]*Tag, error)
	// DeleteTag removes a tag pointer (no-op if absent).
	DeleteTag(ctx context.Context, repoID, tag string) error
}

// newID generates a prefixed, crypto-random identifier ("repo_…").
func newID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

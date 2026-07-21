// Package registryauthz is the wiring glue that binds the reused authorization
// primitives (acl.CanAccess, org membership, hosted repo ownership) to the three
// registry seams that need per-repo decisions:
//
//   - registrytoken.ScopeAuthorizer — GrantedActions, the REAL acl decision made
//     at /token time from the full authenticated principal (who, which org).
//   - registry.Authorizer            — Authorize, the data-plane write chokepoint
//     that trusts the already-issued token scope and lazily creates the org-owned
//     repo row on a first push.
//   - oci.HostedResolver + oci.HostedReadAuthz — the hosted-first pull seam: is a
//     name an org-owned hosted repo, and may the caller read it.
//
// One Authz value implements all four interfaces (the method sets are disjoint),
// closing over the org and repo stores. The registry namespace ↔ org binding is
// "the first path segment of <org>/<repo> is the org" (resolved by slug, then by
// id). Everything fails closed: an unknown namespace, a missing repo on pull, or
// an anonymous caller yields no access.
package registryauthz

import (
	"context"
	"errors"
	"strings"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/registrytoken"
	"github.com/ivanzzeth/specula/internal/repo"
)

// Authz is the shared registry authorization glue. It is safe for concurrent
// use (the underlying stores are).
type Authz struct {
	orgs  org.Store
	repos repo.RepoStore
}

// New constructs the Authz glue over the org and hosted-repo stores.
func New(orgs org.Store, repos repo.RepoStore) *Authz {
	return &Authz{orgs: orgs, repos: repos}
}

// Compile-time assertions: one value satisfies every registry seam.
var (
	_ registrytoken.ScopeAuthorizer = (*Authz)(nil)
	_ registry.Authorizer           = (*Authz)(nil)
	_ oci.HostedResolver            = (*Authz)(nil)
	_ oci.HostedReadAuthz           = (*Authz)(nil)
	_ oci.OwnedNamespaceResolver    = (*Authz)(nil)
)

// namespaceOf returns the org-namespace segment of a repository name
// ("<org>/<repo>[/…]" → "<org>"). A name with no '/' has no namespace.
func namespaceOf(repoName string) string {
	if i := strings.IndexByte(repoName, '/'); i > 0 {
		return repoName[:i]
	}
	return ""
}

// resolveOrgID maps a registry namespace to its owning org id, trying the org
// slug first and then a literal org id. ok=false for an unknown namespace.
func (a *Authz) resolveOrgID(ctx context.Context, namespace string) (string, bool) {
	if namespace == "" || a.orgs == nil {
		return "", false
	}
	if o, err := a.orgs.GetOrgBySlug(ctx, namespace); err == nil && o != nil {
		return o.ID, true
	}
	if o, err := a.orgs.GetOrg(ctx, namespace); err == nil && o != nil {
		return o.ID, true
	}
	return "", false
}

// subjectForOrg builds the acl.Subject a principal presents when acting on a
// resource owned by orgID:
//   - anonymous / identity-less  → empty Subject (public-read only).
//   - API key (pinned OrgID)     → Subject{UserID:subject, OrgID:pinned org}.
//   - password user              → Subject{UserID:subject, OrgID:orgID} when the
//     user is a member of orgID, else an org-less Subject (owner/public only).
func (a *Authz) subjectForOrg(ctx context.Context, p registrytoken.Principal, orgID string) acl.Subject {
	if p.Anonymous || p.Subject == "" {
		return acl.Subject{}
	}
	if p.OrgID != "" {
		return acl.Subject{UserID: p.Subject, OrgID: p.OrgID}
	}
	if a.orgs != nil && p.Email != "" {
		if m, err := a.orgs.GetOrgMember(ctx, orgID, p.Email); err == nil && m != nil {
			return acl.Subject{UserID: p.Subject, OrgID: orgID}
		}
	}
	return acl.Subject{UserID: p.Subject}
}

// resourceFor resolves the acl.Resource for a repo name in orgID. An existing
// hosted repo maps via repo.ToACLResource; a not-yet-created repo is modelled as
// an org-writable resource so an org-write member can create/push it (a
// cross-org caller still fails the same-org check).
func (a *Authz) resourceFor(ctx context.Context, orgID, repoName string) acl.Resource {
	if r, err := a.repos.GetRepo(ctx, orgID, repoName); err == nil && r != nil {
		return r.ToACLResource()
	}
	return acl.Resource{OrgID: orgID, Visibility: acl.Org, Access: acl.Write}
}

// ── registrytoken.ScopeAuthorizer ─────────────────────────────────────────────

// GrantedActions returns the subset of requested actions the principal may
// perform on repoName, using the authoritative acl decision. This is where
// visibility, ownership, org membership and anonymity are actually enforced; the
// data-plane middleware and Authorizer then trust the issued token scope.
func (a *Authz) GrantedActions(ctx context.Context, p registrytoken.Principal, repoName string, requested []string) []string {
	orgID, ok := a.resolveOrgID(ctx, namespaceOf(repoName))
	if !ok {
		// Not a hosted-org namespace: this is a pull-through / upstream mirror
		// name (e.g. "library/nginx"). Grant pull only — the cache proxy serves
		// public upstream content to anyone — and never push/delete (you cannot
		// create a hosted repo outside an org namespace). This keeps anonymous
		// pull-through working once the writable registry gates /v2/.
		return pullOnly(requested)
	}
	resource := a.resourceFor(ctx, orgID, repoName)
	subject := a.subjectForOrg(ctx, p, orgID)

	var granted []string
	for _, action := range requested {
		// API-key principals are further constrained by per-key scopes
		// (pull/push). Password users have empty KeyScopes and skip this gate.
		if len(p.KeyScopes) > 0 && !apikey.AllowsAction(p.KeyScopes, action) {
			continue
		}
		needWrite := action != registrytoken.ActionPull
		if acl.CanAccess(resource, subject, needWrite) == nil {
			granted = append(granted, action)
		}
	}
	return granted
}

// pullOnly returns the pull action from requested (dropping push/delete), used
// to authorize anonymous pull-through of non-hosted upstream names.
func pullOnly(requested []string) []string {
	var granted []string
	for _, action := range requested {
		if action == registrytoken.ActionPull {
			granted = append(granted, action)
		}
	}
	return granted
}

// ── registry.Authorizer ───────────────────────────────────────────────────────

// Authorize is the data-plane write chokepoint. It trusts the token scope
// already verified by the /v2/ Bearer middleware (defense-in-depth re-check of
// the claims) and resolves the org-owned repo, creating it on a first push.
//
// The action re-checked here is the one the request actually needs, matching the
// action the middleware challenged for. Checking a DELETE against "push" would
// deny a correctly scoped delete, because the client's token carries only the
// delete grant it was challenged for.
//
// Permission is decided BEFORE existence: a caller without the grant gets
// acl.ErrForbidden (403); a caller who holds the grant but names a repo that
// does not exist gets repo.ErrNotFound (404) — an authorized caller must never
// be told "forbidden" merely because the row is missing (OCI Distribution
// expects 202/404/405 on delete, never 403).
func (a *Authz) Authorize(ctx context.Context, repoName, action string) (*repo.Repo, error) {
	claims, ok := registrytoken.ClaimsFromContext(ctx)
	if !ok || claims == nil {
		return nil, acl.ErrForbidden // no verified token present
	}
	if !claims.Allows(repoName, action) {
		return nil, acl.ErrForbidden
	}

	orgID, ok := a.resolveOrgID(ctx, namespaceOf(repoName))
	if !ok {
		return nil, repo.ErrNotFound
	}

	r, err := a.repos.GetRepo(ctx, orgID, repoName)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, err
	}
	// Only a push may bring a repo into existence. A pull or delete against a
	// missing repo is a 404 for an authorized caller.
	if action != registrytoken.ActionPush {
		return nil, repo.ErrNotFound
	}

	// First push: create the org-owned repo (default private), owned by the
	// pushing subject. Concurrent first-push blob uploads race here; on a unique
	// (org_id, name) conflict re-read the winner rather than failing the push.
	created, cerr := a.repos.CreateRepo(ctx, orgID, repoName, repo.VisibilityPrivate, claims.Subject)
	if cerr != nil {
		if r2, gerr := a.repos.GetRepo(ctx, orgID, repoName); gerr == nil && r2 != nil {
			return r2, nil
		}
		return nil, cerr
	}
	return created, nil
}

// ── oci.HostedResolver ─────────────────────────────────────────────────────────

// ResolveHosted reports whether name is an org-owned hosted repo. A hosted repo
// is served from CAS and never fetched from an upstream; a non-hosted name falls
// through to the pull-through cache path.
func (a *Authz) ResolveHosted(ctx context.Context, name string) (bool, error) {
	orgID, ok := a.resolveOrgID(ctx, namespaceOf(name))
	if !ok {
		return false, nil
	}
	_, err := a.repos.GetRepo(ctx, orgID, name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// ── oci.OwnedNamespaceResolver ─────────────────────────────────────────────────

// IsOwnedNamespace reports whether name's namespace segment ("<org>/…") maps to
// a known org. Content under an owned namespace is authoritative-local: the OCI
// handler serves it from CAS or 404s, and never falls through to a configured
// upstream mirror (which would break a first push's HEAD-blob existence check
// and leak the org name upstream). A name outside every org namespace (e.g.
// "library/nginx") is a pull-through mirror name and is not owned.
func (a *Authz) IsOwnedNamespace(ctx context.Context, name string) (bool, error) {
	_, ok := a.resolveOrgID(ctx, namespaceOf(name))
	return ok, nil
}

// ── oci.HostedReadAuthz ────────────────────────────────────────────────────────

// AuthorizeRead enforces per-repo pull visibility for hosted repos (called by
// the OCI handler after ResolveHosted confirms a repo is hosted). It honours the
// token scope verified upstream and, as a fallback, allows a public repo. No
// claims → ErrUnauthorized (401); claims present but insufficient → ErrForbidden
// (403).
func (a *Authz) AuthorizeRead(ctx context.Context, repoName string) error {
	claims, ok := registrytoken.ClaimsFromContext(ctx)
	if ok && claims != nil && claims.Allows(repoName, registrytoken.ActionPull) {
		return nil
	}
	// Fallback: a public hosted repo is readable by anyone (including a caller
	// whose token predates a private→public flip).
	if orgID, resolved := a.resolveOrgID(ctx, namespaceOf(repoName)); resolved {
		if r, err := a.repos.GetRepo(ctx, orgID, repoName); err == nil && r != nil {
			if repo.NormalizeVisibility(r.Visibility) == repo.VisibilityPublic {
				return nil
			}
		}
	}
	if !ok || claims == nil {
		return oci.ErrUnauthorized
	}
	return oci.ErrForbidden
}

// ── registrytoken.PasswordVerifier ─────────────────────────────────────────────

// PasswordAuth adapts auth.Service into a registrytoken.PasswordVerifier so
// `docker login -u <email> -p <password>` (as opposed to an API key) also works
// against /token. It returns the caller's acl user-subject ("user:<id>").
type PasswordAuth struct {
	Svc *auth.Service
}

// Compile-time assertion.
var _ registrytoken.PasswordVerifier = (*PasswordAuth)(nil)

// VerifyPassword verifies email:password via auth.Service.Login.
func (p *PasswordAuth) VerifyPassword(ctx context.Context, email, password string) (string, bool) {
	if p.Svc == nil {
		return "", false
	}
	_, u, err := p.Svc.Login(ctx, email, password)
	if err != nil || u == nil {
		return "", false
	}
	return org.UserSubjectID(u.ID), true
}

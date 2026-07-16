package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
)

// grantResourceTypeRepo is the resource_grants.resource_type value for hosted
// repositories (cross-org sharing).
const grantResourceTypeRepo = "repo"

// repoNameCandidates lists the stored repo names that a {org}/{repo} path pair
// could refer to, in priority order.
//
// Repo names are stored fully qualified ("<namespace>/<repo>") because that is
// exactly what a pull reference uses after the host. The namespace recorded at
// push time is whatever the pushing client typed, and registryauthz resolves a
// namespace by org slug OR by org id — so the same org can legitimately own both
// "acme/app" (pushed as acme) and "org_1/app" (pushed by id). A UI navigating by
// slug must still find the latter, so every plausible namespace is tried.
//
// The {repo} segment is a single path segment (Go 1.22 routing does not let a
// non-terminal wildcard span "/"), i.e. the bare repo name. A repo whose name
// nests further ("acme/team/app") is therefore not addressable through this API.
func repoNameCandidates(o *org.Org, orgRef, repoSeg string) []string {
	repoSeg = strings.Trim(repoSeg, "/")
	if repoSeg == "" {
		return nil
	}
	var out []string
	seen := make(map[string]struct{}, 3)
	for _, ns := range []string{orgRef, o.Slug, o.ID} {
		if ns == "" {
			continue
		}
		name := ns + "/" + repoSeg
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// subjectFor builds the acl.Subject for the calling principal acting on a
// resource owned by orgID.
//
// This mirrors registryauthz.subjectForOrg deliberately: the WebUI and the
// docker client must reach the SAME authorization answer for the same repo, so
// both paths must present the principal to acl the same way. An API key carries
// its own pinned org; a session user is presented as a member of orgID only when
// they really are one.
func (s *Server) subjectFor(r *http.Request, orgID string) acl.Subject {
	subj, _ := auth.SubjectFromContext(r.Context())
	if subj.UserID == "" {
		return acl.Subject{} // anonymous: public-read only
	}
	if subj.OrgID != "" {
		return subj
	}
	if u, ok := auth.UserFromContext(r.Context()); ok && s.orgs != nil {
		if m, err := s.orgs.GetOrgMember(r.Context(), orgID, u.Email); err == nil && m != nil {
			subj.OrgID = orgID
		}
	}
	return subj
}

// orgRoleOf returns the caller's effective org role in orgID, or "" when they
// have none.
//
// It defers to the role PrincipalMiddleware already resolved for the active org,
// which is the only place that knows about a system-role holder's implicit
// read-only view. Re-deriving the role from membership alone silently dropped
// that view: a system viewer could list an org's repos but got 404 on any single
// one of them — read-only access that could not actually read.
func (s *Server) orgRoleOf(r *http.Request, orgID string) string {
	if active, ok := auth.ActiveOrgFromContext(r.Context()); ok && active.ID == orgID {
		return active.Role
	}
	u, ok := auth.UserFromContext(r.Context())
	if !ok || s.orgs == nil {
		return ""
	}
	m, err := s.orgs.GetOrgMember(r.Context(), orgID, u.Email)
	if err != nil || m == nil {
		return ""
	}
	return org.NormalizeRole(m.Role)
}

// authorizeRepo decides whether the caller may read (needWrite=false) or
// administer (needWrite=true) a hosted repo, on two deliberate axes:
//
//  1. acl.CanAccessGranted — the authoritative per-resource decision: system
//     admin, the repo's owner, public read, and cross-org grants. This is the
//     same chokepoint the registry data plane uses, so the WebUI can never show
//     a repo that `docker pull` would refuse (or vice versa).
//
//  2. The org role ladder — a repo belongs to an org, and administering an org's
//     repos is an org-RBAC concern, not a per-resource one. acl models a repo as
//     private|public with a single owner, so without this axis an org admin
//     could not flip the visibility of a repo a teammate pushed, and org members
//     could not see their own org's private repos in the UI. Read needs viewer+;
//     write needs admin+ (matching the members API and REGISTRY-DESIGN §5.1's
//     "visibility toggle (owner/admin)").
//
// Neither axis is an inlined visibility check: axis 1 delegates entirely to acl,
// and axis 2 is org membership, which acl does not model.
//
// It reports ok=false having already written the response.
func (s *Server) authorizeRepo(w http.ResponseWriter, r *http.Request, orgID string, rp *repo.Repo, needWrite bool) bool {
	subject := s.subjectFor(r, orgID)

	var granted []string
	if s.grants != nil && rp != nil {
		granted = s.grants.GrantedOrgs(grantResourceTypeRepo, rp.ID)
	}
	if acl.CanAccessGranted(rp.ToACLResource(), subject, needWrite, granted) == nil {
		return true
	}

	role := s.orgRoleOf(r, orgID)
	need := org.RoleViewer
	if needWrite {
		need = org.RoleAdmin
	}
	if role != "" && org.AtLeast(role, need) {
		return true
	}

	// Fail closed. A caller who cannot even read the repo is told 404 rather
	// than 403: "forbidden" on a private repo confirms it exists, which leaks
	// the org's repo names to anyone who can guess them.
	if !needWrite {
		writeError(w, http.StatusNotFound, "repo not found")
		return false
	}
	// For a write, the caller has already passed the read check at the call
	// site, so the repo's existence is not a secret from them.
	writeError(w, http.StatusForbidden, "org admin role or repo ownership required")
	return false
}

// requireOrgMember ensures the caller belongs to orgID (any role), or is a
// system admin. Used by the repo LIST endpoint, which is an org-scoped view
// rather than a per-resource one.
func (s *Server) requireOrgMember(w http.ResponseWriter, r *http.Request, orgID string) bool {
	subj, _ := auth.SubjectFromContext(r.Context())
	if subj.Admin {
		return true
	}
	// An API key pinned to this org acts on the org's behalf.
	if subj.OrgID != "" && subj.OrgID == orgID {
		return true
	}
	if role := s.orgRoleOf(r, orgID); role != "" {
		return true
	}
	writeError(w, http.StatusForbidden, "not a member of this org")
	return false
}

// resolveOrg maps the {org} path segment (slug or id) to its org record, so the
// UI may use whichever it holds. Reports ok=false having written the response.
func (s *Server) resolveOrg(w http.ResponseWriter, r *http.Request, ref string) (*org.Org, bool) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return nil, false
	}
	if o, err := s.orgs.GetOrgBySlug(r.Context(), ref); err == nil && o != nil {
		return o, true
	}
	if o, err := s.orgs.GetOrg(r.Context(), ref); err == nil && o != nil {
		return o, true
	}
	writeError(w, http.StatusNotFound, "org not found")
	return nil, false
}

// loadRepo fetches a repo by org + path segment, writing a 404 when absent.
// It tries each plausible stored name (see repoNameCandidates).
func (s *Server) loadRepo(w http.ResponseWriter, r *http.Request, o *org.Org, orgRef, repoSeg string) (*repo.Repo, bool) {
	for _, name := range repoNameCandidates(o, orgRef, repoSeg) {
		rp, err := s.repos.GetRepo(r.Context(), o.ID, name)
		if err == nil {
			return rp, true
		}
		if !errors.Is(err, repo.ErrNotFound) {
			s.log.Error("admin: get repo", "err", err, "org_id", o.ID, "repo", name)
			writeError(w, http.StatusInternalServerError, "failed to load repo")
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "repo not found")
	return nil, false
}

// toRepoDTO projects a repo row, enriching it with tag aggregates when a tag
// store is wired in.
func (s *Server) toRepoDTO(r *http.Request, rp *repo.Repo) RepoDTO {
	dto := RepoDTO{
		ID:          rp.ID,
		OrgID:       rp.OrgID,
		Name:        rp.Name,
		Visibility:  repo.NormalizeVisibility(rp.Visibility),
		OwnerUserID: rp.OwnerUserID,
		CreatedAt:   rp.CreatedAt,
	}
	if s.tags == nil {
		return dto
	}
	tags, err := s.tags.ListTags(r.Context(), rp.ID)
	if err != nil {
		// A tag-aggregate failure must not fail the repo listing; report the
		// repo without aggregates rather than 500-ing the whole page.
		s.log.Warn("admin: list tags for repo aggregate", "err", err, "repo_id", rp.ID)
		return dto
	}
	dto.TagCount = len(tags)
	for _, t := range tags {
		if t.UpdatedAt.After(dto.LastPushedAt) {
			dto.LastPushedAt = t.UpdatedAt
		}
		// Sum the tagged MANIFEST sizes only — see RepoDTO.SizeBytes: the layer
		// blobs are not walked, so this is not the image pull size.
		dto.SizeBytes += s.manifestSize(r, rp.Name, t.Digest)
	}
	return dto
}

// manifestSize looks up a manifest's recorded byte size in the cache metadata,
// returning 0 when there is no record (rather than guessing).
func (s *Server) manifestSize(r *http.Request, repoName, digest string) int64 {
	if s.meta == nil || digest == "" {
		return 0
	}
	e, err := s.meta.Get(r.Context(), artifact.ArtifactRef{
		Protocol: "oci",
		Name:     repoName,
		Version:  digest,
	})
	if err != nil || e == nil {
		return 0
	}
	return e.Size
}

// ── handlers ─────────────────────────────────────────────────────────────────

// handleListRepos → GET /api/v1/orgs/{org}/repos. Returns ReposResponse.
func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	if !s.requireOrgMember(w, r, o.ID) {
		return
	}

	repos, err := s.repos.ListRepos(r.Context(), o.ID)
	if err != nil {
		s.log.Error("admin: list repos", "err", err, "org_id", o.ID)
		writeError(w, http.StatusInternalServerError, "failed to list repos")
		return
	}
	dtos := make([]RepoDTO, 0, len(repos))
	for _, rp := range repos {
		dtos = append(dtos, s.toRepoDTO(r, rp))
	}
	writeJSON(w, http.StatusOK, ReposResponse{Repos: dtos})
}

// handleGetRepo → GET /api/v1/orgs/{org}/repos/{repo}. Returns RepoDTO.
func (s *Server) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, r.PathValue("repo"))
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, false) {
		return
	}
	writeJSON(w, http.StatusOK, s.toRepoDTO(r, rp))
}

// handlePatchRepo → PATCH /api/v1/orgs/{org}/repos/{repo}. Returns RepoDTO.
//
// This is the endpoint the docker real-client test had to work around by
// writing SQLite directly: flipping a repo private↔public is a first-class,
// authorized operation, not a DB poke.
func (s *Server) handlePatchRepo(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, r.PathValue("repo"))
	if !ok {
		return
	}
	// Read first so a caller who may not even see the repo gets 404, not 403.
	if !s.authorizeRepo(w, r, o.ID, rp, false) {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}

	var req PatchRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Visibility == nil {
		writeJSON(w, http.StatusOK, s.toRepoDTO(r, rp)) // nothing to change
		return
	}
	// Reject rather than normalize: repo.NormalizeVisibility would silently map
	// a typo like "publicc" to private. A visibility change is a security
	// decision — an unrecognised value must fail loudly, not fail quietly.
	vis := *req.Visibility
	if vis != repo.VisibilityPrivate && vis != repo.VisibilityPublic {
		writeError(w, http.StatusBadRequest, `visibility must be "private" or "public"`)
		return
	}
	if err := s.repos.SetVisibility(r.Context(), o.ID, rp.Name, vis); err != nil {
		s.log.Error("admin: set repo visibility", "err", err, "repo", rp.Name)
		writeError(w, http.StatusInternalServerError, "failed to update visibility")
		return
	}
	s.log.Info("admin: repo visibility changed",
		"repo", rp.Name, "org_id", o.ID, "from", rp.Visibility, "to", vis)

	rp.Visibility = vis
	writeJSON(w, http.StatusOK, s.toRepoDTO(r, rp))
}

// handleDeleteRepo → DELETE /api/v1/orgs/{org}/repos/{repo}. Returns 204.
func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, r.PathValue("repo"))
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, false) {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}

	if err := s.repos.DeleteRepo(r.Context(), o.ID, rp.Name); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent) // already gone: idempotent
			return
		}
		s.log.Error("admin: delete repo", "err", err, "repo", rp.Name)
		writeError(w, http.StatusInternalServerError, "failed to delete repo")
		return
	}
	s.log.Info("admin: repo deleted", "repo", rp.Name, "org_id", o.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleListRepoTags → GET /api/v1/orgs/{org}/repos/{repo}/tags.
// Returns TagsResponse.
func (s *Server) handleListRepoTags(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil || s.tags == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, r.PathValue("repo"))
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, false) {
		return
	}

	tags, err := s.tags.ListTags(r.Context(), rp.ID)
	if err != nil {
		s.log.Error("admin: list tags", "err", err, "repo_id", rp.ID)
		writeError(w, http.StatusInternalServerError, "failed to list tags")
		return
	}
	dtos := make([]TagDTO, 0, len(tags))
	for _, t := range tags {
		dtos = append(dtos, TagDTO{
			Tag:      t.Tag,
			Digest:   t.Digest,
			Size:     s.manifestSize(r, rp.Name, t.Digest),
			PushedAt: t.UpdatedAt,
			// Arch is intentionally left empty: nothing parses the image config
			// blob, so there is no architecture to report. See TagDTO.Arch.
		})
	}
	writeJSON(w, http.StatusOK, TagsResponse{Tags: dtos})
}

// handleDeleteRepoTag → DELETE /api/v1/orgs/{org}/repos/{repo}/tags/{tag}.
// Returns 204.
//
// This removes the tag pointer only. The manifest and its layer blobs stay in
// the shared CAS: they are content-addressed and may be referenced by other
// tags or repos, so deleting them here could corrupt an unrelated image.
// Reclaiming unreferenced blobs is GC's job.
func (s *Server) handleDeleteRepoTag(w http.ResponseWriter, r *http.Request) {
	if s.repos == nil || s.tags == nil {
		writeError(w, http.StatusNotImplemented, "repo store not configured")
		return
	}
	orgRef := r.PathValue("org")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, r.PathValue("repo"))
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, false) {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}

	tag := r.PathValue("tag")
	if err := s.tags.DeleteTag(r.Context(), rp.ID, tag); err != nil {
		s.log.Error("admin: delete tag", "err", err, "repo_id", rp.ID, "tag", tag)
		writeError(w, http.StatusInternalServerError, "failed to delete tag")
		return
	}
	s.log.Info("admin: tag deleted", "repo", rp.Name, "tag", tag)
	w.WriteHeader(http.StatusNoContent)
}

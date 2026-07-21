package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/grant"
	"github.com/ivanzzeth/specula/internal/repo"
)

// GrantDTO is one cross-org (or per-user) share on a hosted repo.
type GrantDTO struct {
	SubjectType string    `json:"subject_type"` // org | user
	SubjectID   string    `json:"subject_id"`
	Access      string    `json:"access"` // read | write
	GrantedBy   string    `json:"granted_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// GrantsResponse is GET …/grants.
type GrantsResponse struct {
	Grants []GrantDTO `json:"grants"`
}

// UpsertGrantRequest is PUT …/grants.
type UpsertGrantRequest struct {
	SubjectType string `json:"subject_type"` // org | user (default org)
	SubjectID   string `json:"subject_id"`   // org id/slug for org; user subject for user
	Access      string `json:"access"`       // read | write (default read)
}

func toGrantDTO(g grant.Grant) GrantDTO {
	return GrantDTO{
		SubjectType: g.SubjectType,
		SubjectID:   g.SubjectID,
		Access:      g.Access,
		GrantedBy:   g.GrantedBy,
		CreatedAt:   g.CreatedAt,
	}
}

// grantAllowsRepo reports whether an explicit resource_grant lets the caller
// read (needWrite=false) or write the repo. Private repos are owner-only in
// acl.CanAccessGranted, so cross-org shares are enforced here.
func (s *Server) grantAllowsRepo(r *http.Request, rp *repo.Repo, needWrite bool) bool {
	if s.grants == nil || rp == nil {
		return false
	}
	for _, orgID := range s.callerOrgIDs(r) {
		if grant.Allows(s.grants.OrgAccess(grantResourceTypeRepo, rp.ID, orgID), needWrite) {
			return true
		}
	}
	return false
}

// callerOrgIDs lists org ids the caller may act as for grant matching: a pinned
// API-key org, or every org the session user belongs to.
func (s *Server) callerOrgIDs(r *http.Request) []string {
	subj, _ := auth.SubjectFromContext(r.Context())
	if subj.OrgID != "" {
		return []string{subj.OrgID}
	}
	if active, ok := auth.ActiveOrgFromContext(r.Context()); ok && active.ID != "" {
		// Prefer active org first, then the rest of memberships.
		seen := map[string]bool{active.ID: true}
		out := []string{active.ID}
		if u, ok := auth.UserFromContext(r.Context()); ok && s.orgs != nil {
			if orgs, err := s.orgs.ListOrgsForEmail(r.Context(), u.Email); err == nil {
				for _, o := range orgs {
					if o != nil && !seen[o.ID] {
						seen[o.ID] = true
						out = append(out, o.ID)
					}
				}
			}
		}
		return out
	}
	u, ok := auth.UserFromContext(r.Context())
	if !ok || s.orgs == nil {
		return nil
	}
	orgs, err := s.orgs.ListOrgsForEmail(r.Context(), u.Email)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(orgs))
	for _, o := range orgs {
		if o != nil && o.ID != "" {
			out = append(out, o.ID)
		}
	}
	return out
}

// resolveGrantSubjectOrg maps subject_id (slug or id) to a stable org id when
// subject_type is org.
func (s *Server) resolveGrantSubjectOrg(r *http.Request, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || s.orgs == nil {
		return "", errGrantSubject
	}
	if o, err := s.orgs.GetOrgBySlug(r.Context(), ref); err == nil && o != nil {
		return o.ID, nil
	}
	if o, err := s.orgs.GetOrg(r.Context(), ref); err == nil && o != nil {
		return o.ID, nil
	}
	return "", errGrantSubject
}

type grantSubjectError struct{}

func (grantSubjectError) Error() string { return "grant subject not found" }

var errGrantSubject = grantSubjectError{}

// handleListRepoGrants → GET /api/v1/orgs/{org}/repos/{repo}/grants
func (s *Server) handleListRepoGrants(w http.ResponseWriter, r *http.Request) {
	if s.grants == nil {
		writeError(w, http.StatusNotImplemented, "grant store not configured")
		return
	}
	orgRef := r.PathValue("org")
	repoSeg := r.PathValue("repo")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, repoSeg)
	if !ok {
		return
	}
	// Listing grants is an admin action on the owning org's repo.
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}
	rows, err := s.grants.Grants(grantResourceTypeRepo, rp.ID)
	if err != nil {
		s.log.Error("admin: list grants", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list grants")
		return
	}
	out := make([]GrantDTO, 0, len(rows))
	for _, g := range rows {
		out = append(out, toGrantDTO(g))
	}
	writeJSON(w, http.StatusOK, GrantsResponse{Grants: out})
}

// handleUpsertRepoGrant → PUT /api/v1/orgs/{org}/repos/{repo}/grants
func (s *Server) handleUpsertRepoGrant(w http.ResponseWriter, r *http.Request) {
	if s.grants == nil {
		writeError(w, http.StatusNotImplemented, "grant store not configured")
		return
	}
	orgRef := r.PathValue("org")
	repoSeg := r.PathValue("repo")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, repoSeg)
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}

	var req UpsertGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	subjType := strings.ToLower(strings.TrimSpace(req.SubjectType))
	if subjType == "" {
		subjType = grant.SubjectOrg
	}
	if subjType != grant.SubjectOrg && subjType != grant.SubjectUser {
		writeError(w, http.StatusBadRequest, "subject_type must be org or user")
		return
	}
	access := strings.ToLower(strings.TrimSpace(req.Access))
	if access == "" {
		access = grant.AccessRead
	}
	if access != grant.AccessRead && access != grant.AccessWrite {
		writeError(w, http.StatusBadRequest, "access must be read or write")
		return
	}
	subjectID := strings.TrimSpace(req.SubjectID)
	if subjectID == "" {
		writeError(w, http.StatusBadRequest, "subject_id is required")
		return
	}
	if subjType == grant.SubjectOrg {
		id, err := s.resolveGrantSubjectOrg(r, subjectID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "grant subject org not found")
			return
		}
		if id == o.ID {
			writeError(w, http.StatusBadRequest, "cannot grant a repo to its owning org")
			return
		}
		subjectID = id
	}

	caller, _ := auth.SubjectFromContext(r.Context())
	g := grant.Grant{
		ResourceType: grantResourceTypeRepo,
		ResourceID:   rp.ID,
		SubjectType:  subjType,
		SubjectID:    subjectID,
		Access:       access,
		GrantedBy:    caller.UserID,
	}
	if err := s.grants.Upsert(g); err != nil {
		s.log.Error("admin: upsert grant", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to upsert grant")
		return
	}
	writeJSON(w, http.StatusOK, toGrantDTO(g))
}

// handleDeleteRepoGrant → DELETE /api/v1/orgs/{org}/repos/{repo}/grants/{stype}/{sid}
func (s *Server) handleDeleteRepoGrant(w http.ResponseWriter, r *http.Request) {
	if s.grants == nil {
		writeError(w, http.StatusNotImplemented, "grant store not configured")
		return
	}
	orgRef := r.PathValue("org")
	repoSeg := r.PathValue("repo")
	stype := strings.ToLower(r.PathValue("stype"))
	sid := r.PathValue("sid")
	o, ok := s.resolveOrg(w, r, orgRef)
	if !ok {
		return
	}
	rp, ok := s.loadRepo(w, r, o, orgRef, repoSeg)
	if !ok {
		return
	}
	if !s.authorizeRepo(w, r, o.ID, rp, true) {
		return
	}
	if stype != grant.SubjectOrg && stype != grant.SubjectUser {
		writeError(w, http.StatusBadRequest, "subject_type must be org or user")
		return
	}
	if stype == grant.SubjectOrg {
		if id, err := s.resolveGrantSubjectOrg(r, sid); err == nil {
			sid = id
		}
	}
	if err := s.grants.Delete(grantResourceTypeRepo, rp.ID, stype, sid); err != nil {
		s.log.Error("admin: delete grant", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to delete grant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
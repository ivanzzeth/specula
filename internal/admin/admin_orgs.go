package admin

// admin_orgs.go — system-admin org lifecycle for platform orchestrators (chorei).
//
// Mirrors ai-sandbox POST/GET /api/v1/admin/orgs:
//   GET  /api/v1/admin/orgs              list all orgs (?slug= filter)
//   POST /api/v1/admin/orgs              create org; optional admin_email → owner
//   POST /api/v1/admin/orgs/{id}/keys    mint an org-scoped API key for any org
//
// Auth: session system_role=admin OR Bearer matching Deps.AdminKey (break-glass,
// same shape as saidbox SANDBOX_ADMIN_KEY). Self-service POST /api/v1/orgs stays
// human-only; these routes are the machine path chorei uses as SoT sync.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
)

// AdminCreateOrgRequest is the POST /api/v1/admin/orgs body.
type AdminCreateOrgRequest struct {
	Name       string `json:"name"`
	Slug       string `json:"slug,omitempty"`
	AdminEmail string `json:"admin_email,omitempty"`
}

// adminKeyMatch compares Bearer token to the configured break-glass AdminKey
// in constant time. Empty configured key never matches (admin-key path off).
func (s *Server) adminKeyMatch(token string) bool {
	want := strings.TrimSpace(s.adminKey)
	got := strings.TrimSpace(token)
	if want == "" || got == "" {
		return false
	}
	if len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// withAdminKeyContext injects a synthetic system-admin User so handlers that
// call UserFromContext keep working under break-glass.
func withAdminKeyContext(r *http.Request) *http.Request {
	u := auth.User{
		Email:      "admin-key@specula.local",
		Name:       "admin-key",
		SystemRole: "admin",
	}
	return r.WithContext(auth.ContextWithUser(r.Context(), u))
}

// requireSystemAdmin allows session system admins OR the configured AdminKey.
func (s *Server) requireSystemAdmin(h http.HandlerFunc) http.Handler {
	authMW := auth.Middleware(s.tokens, s.users)
	sessionAdmin := authMW(auth.AdminRequired(h))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := tokenFromRequest(r)
		if s.adminKeyMatch(token) {
			h(w, withAdminKeyContext(r))
			return
		}
		sessionAdmin.ServeHTTP(w, r)
	})
}

// handleAdminListOrgs → GET /api/v1/admin/orgs (?slug= optional filter).
func (s *Server) handleAdminListOrgs(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	if slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slug"))); slug != "" {
		o, err := s.orgs.GetOrgBySlug(r.Context(), slug)
		if err != nil {
			if errors.Is(err, org.ErrNotFound) {
				writeJSON(w, http.StatusOK, OrgsResponse{Orgs: []OrgDTO{}})
				return
			}
			s.log.Error("admin: get org by slug", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to look up org")
			return
		}
		writeJSON(w, http.StatusOK, OrgsResponse{Orgs: []OrgDTO{toOrgDTO(*o)}})
		return
	}
	orgList, err := s.orgs.ListOrgs(r.Context())
	if err != nil {
		s.log.Error("admin: list orgs", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list orgs")
		return
	}
	dtos := make([]OrgDTO, 0, len(orgList))
	for _, o := range orgList {
		dtos = append(dtos, toOrgDTO(*o))
	}
	writeJSON(w, http.StatusOK, OrgsResponse{Orgs: dtos})
}

// handleAdminCreateOrg → POST /api/v1/admin/orgs.
// Creator is NOT auto-added as member. Optional admin_email seeds founding owner.
func (s *Server) handleAdminCreateOrg(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	var req AdminCreateOrgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		writeError(w, http.StatusBadRequest, "name must contain at least one letter or digit")
		return
	}
	if existing, err := s.orgs.GetOrgBySlug(r.Context(), slug); err == nil && existing != nil {
		writeError(w, http.StatusConflict, "org slug already exists")
		return
	} else if err != nil && !errors.Is(err, org.ErrNotFound) {
		s.log.Error("admin: slug conflict check", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to check org slug")
		return
	}

	createdBy := "admin-key"
	if u, ok := auth.UserFromContext(r.Context()); ok && u.ID != 0 {
		createdBy = org.UserSubjectID(u.ID)
	}
	now := time.Now().UTC()
	newOrg := &org.Org{
		Name:      name,
		Slug:      slug,
		Status:    org.StatusActive,
		CreatedBy: createdBy,
		CreatedAt: now,
	}
	if err := s.orgs.CreateOrg(r.Context(), newOrg); err != nil {
		s.log.Error("admin: create org", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create org")
		return
	}
	if email := strings.ToLower(strings.TrimSpace(req.AdminEmail)); email != "" {
		if err := s.orgs.AddOrgMember(r.Context(), &org.Member{
			OrgID:     newOrg.ID,
			Email:     email,
			Role:      org.RoleOwner,
			CreatedAt: now,
		}); err != nil {
			s.log.Error("admin: seed org owner", "err", err, "org_id", newOrg.ID)
			writeError(w, http.StatusInternalServerError, "failed to assign org owner")
			return
		}
	}
	writeJSON(w, http.StatusCreated, toOrgDTO(*newOrg))
}

// handleAdminCreateOrgKey → POST /api/v1/admin/orgs/{id}/keys.
func (s *Server) handleAdminCreateOrgKey(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeError(w, http.StatusNotImplemented, "key store not configured")
		return
	}
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	if orgID == "" {
		writeError(w, http.StatusBadRequest, "org id required")
		return
	}
	if _, err := s.orgs.GetOrg(r.Context(), orgID); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "cannot look up the organization right now")
		return
	}
	var req CreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id, rawKey, err := s.keys.Create(orgID, req.Label, req.Scopes...)
	if err != nil {
		s.log.Error("admin: create org key", "err", err, "org_id", orgID)
		writeError(w, http.StatusInternalServerError, "failed to create key")
		return
	}
	info, ok := s.keys.Get(orgID, id)
	if !ok {
		writeError(w, http.StatusInternalServerError, "key created but not found")
		return
	}
	writeJSON(w, http.StatusCreated, toKeyDTO(info, rawKey))
}

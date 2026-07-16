package admin

// invitations.go is the org membership invitation lifecycle, ported from
// ai-sandbox internal/controlplane/api/org_invitations.go:
//
//	POST   /api/v1/orgs/{id}/invitations   body{email,role} -> create pending invite (org admin+), returns token
//	GET    /api/v1/orgs/{id}/invitations                    -> list this org's invites (org admin+; never returns tokens)
//	PATCH  /api/v1/invitations/{token}     body{status}      -> invitee accepts/declines
//	POST   /api/v1/invitations/accept      body{token}       -> legacy accept alias
//	DELETE /api/v1/orgs/{id}/members/me                      -> leave the org (last-owner guard)
//
// The load-bearing invariant: creating an invitation NEVER writes an
// org_members row. Only the invitee accepting does, with invited_by backfilled
// from the invitation. Membership is the authorization boundary for pushes, so
// an invite that auto-joined would hand write access to an un-consenting party.
//
// Email delivery is out of scope: the creation response carries the token
// directly (a stand-in for the email a production deployment would send).
// Without it the invitee has no way to accept — which is exactly how the
// invitation flow was unusable before this file existed.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
)

// inviteTTL is how long an invitation stays acceptable (7 days, matching
// ai-sandbox). An invitation created without an expiry is a credential that
// never dies; expiry is enforced lazily on accept/decline.
const inviteTTL = 7 * 24 * time.Hour

// newInviteToken mints a high-entropy invitation token (24 bytes = 192 bits from
// crypto/rand, URL-safe unpadded so it can live in a link).
func newInviteToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// handleCreateInvitation → POST /api/v1/orgs/{id}/invitations. 201 + InvitationDTO
// including the token. Requires org admin+ on the path org.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	var req CreateInvitationRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	role, ok := inviteRole(w, req.Role)
	if !ok {
		return
	}

	expires := req.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().UTC().Add(inviteTTL)
	}

	subj, _ := auth.SubjectFromContext(r.Context())
	inv := &org.Invitation{
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		InvitedBy: subj.UserID,
		Token:     newInviteToken(),
		Status:    org.InviteStatusPending,
		ExpiresAt: expires,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.orgs.CreateInvitation(r.Context(), inv); err != nil {
		s.log.Error("admin: create invitation", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create invitation")
		return
	}
	writeJSON(w, http.StatusCreated, toInvitationDTO(*inv, true))
}

// inviteRole validates and normalises a requested invitation role. Empty means
// least privilege (viewer); an unrecognised value is rejected rather than
// silently downgraded, so a typo'd "Admin" cannot read as "viewer". owner is
// never grantable by invitation — ownership must be conferred by a sitting
// owner through the member endpoint.
func inviteRole(w http.ResponseWriter, raw string) (string, bool) {
	switch trimmed := strings.ToLower(strings.TrimSpace(raw)); trimmed {
	case "":
		return org.RoleViewer, true
	case org.RoleOwner:
		writeError(w, http.StatusForbidden, "owner cannot be granted via invitation")
		return "", false
	case org.RoleViewer, org.RoleEditor, org.RoleAdmin:
		return trimmed, true
	default:
		if normalized := org.NormalizeLegacyRole(trimmed); normalized != "" {
			return normalized, true
		}
		writeError(w, http.StatusBadRequest, "role must be viewer, editor or admin")
		return "", false
	}
}

// handleListInvitations → GET /api/v1/orgs/{id}/invitations. Requires org admin+.
// Tokens are withheld: the list is a management view, not a delivery channel —
// leaking them here would let any org admin accept on someone else's behalf.
func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}
	invs, err := s.orgs.ListInvitations(r.Context(), orgID)
	if err != nil {
		s.log.Error("admin: list invitations", "err", err, "org_id", orgID)
		writeError(w, http.StatusInternalServerError, "failed to list invitations")
		return
	}
	out := make([]InvitationDTO, 0, len(invs))
	for _, inv := range invs {
		out = append(out, toInvitationDTO(*inv, false))
	}
	writeJSON(w, http.StatusOK, InvitationsResponse{Invitations: out})
}

// resolvePendingInvite locates the invitation by token and runs the checks
// common to accept and decline: caller must be a human, the token must hit, the
// invitation must not be expired (lazily converged to "expired" → 410), must
// still be pending (409), and must be addressed to the caller (403).
func (s *Server) resolvePendingInvite(w http.ResponseWriter, r *http.Request, token string) (*org.Invitation, string, bool) {
	u, isUser := auth.UserFromContext(r.Context())
	if !isUser {
		writeError(w, http.StatusForbidden, "human login required to respond to an invitation")
		return nil, "", false
	}
	if strings.TrimSpace(token) == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return nil, "", false
	}

	inv, err := s.orgs.GetInvitationByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invitation not found")
			return nil, "", false
		}
		s.log.Error("admin: get invitation by token", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to get invitation")
		return nil, "", false
	}

	// Lazy expiry convergence: a lapsed invitation is retired on touch so it
	// cannot sit "pending" forever.
	if inv.Expired() {
		if inv.Status == org.InviteStatusPending {
			if err := s.orgs.SetInvitationStatus(r.Context(), inv.ID, org.InviteStatusExpired); err != nil {
				s.log.Error("admin: expire invitation", "err", err, "id", inv.ID)
			}
		}
		writeError(w, http.StatusGone, "invitation has expired")
		return nil, "", false
	}
	if inv.Status != org.InviteStatusPending {
		writeError(w, http.StatusConflict, "invitation is no longer pending")
		return nil, "", false
	}

	callerEmail := strings.ToLower(strings.TrimSpace(u.Email))
	if callerEmail != inv.Email {
		writeError(w, http.StatusForbidden, "invitation is addressed to a different email")
		return nil, "", false
	}
	return inv, callerEmail, true
}

// handlePatchInvitation → PATCH /api/v1/invitations/{token} body{status}.
// The invitation's status is a property of the resource, so it is PATCHed
// rather than poked with action endpoints.
func (s *Server) handlePatchInvitation(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	var req PatchInvitationRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	switch req.Status {
	case org.InviteStatusAccepted:
		s.acceptInvitation(w, r, r.PathValue("token"))
	case org.InviteStatusDeclined:
		s.declineInvitation(w, r, r.PathValue("token"))
	default:
		writeError(w, http.StatusBadRequest, "status must be 'accepted' or 'declined'")
	}
}

// handleAcceptInvitation → POST /api/v1/invitations/accept body{token}.
// Retained as an alias for the canonical PATCH so existing clients keep working.
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	var req AcceptInvitationRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	s.acceptInvitation(w, r, req.Token)
}

// acceptInvitation writes the org_members row (invited_by backfilled from the
// invitation) and marks the invitation accepted. This is the ONLY path that
// creates a membership from an invitation.
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request, token string) {
	inv, callerEmail, ok := s.resolvePendingInvite(w, r, token)
	if !ok {
		return
	}
	m := &org.Member{
		OrgID:     inv.OrgID,
		Email:     callerEmail,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.orgs.AddOrgMember(r.Context(), m); err != nil {
		s.log.Error("admin: accept invitation add member", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to add member")
		return
	}
	// Only flip to accepted once the membership is durable — the reverse order
	// would burn the token while leaving the invitee outside the org.
	if err := s.orgs.SetInvitationStatus(r.Context(), inv.ID, org.InviteStatusAccepted); err != nil {
		s.log.Error("admin: accept invitation set status", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update invitation")
		return
	}
	if added, err := s.orgs.GetOrgMember(r.Context(), inv.OrgID, callerEmail); err == nil {
		writeJSON(w, http.StatusOK, toMemberDTO(*added))
		return
	}
	writeJSON(w, http.StatusOK, toMemberDTO(*m))
}

// declineInvitation marks the invitation declined and writes no membership.
func (s *Server) declineInvitation(w http.ResponseWriter, r *http.Request, token string) {
	inv, _, ok := s.resolvePendingInvite(w, r, token)
	if !ok {
		return
	}
	if err := s.orgs.SetInvitationStatus(r.Context(), inv.ID, org.InviteStatusDeclined); err != nil {
		s.log.Error("admin: decline invitation", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update invitation")
		return
	}
	inv.Status = org.InviteStatusDeclined
	writeJSON(w, http.StatusOK, toInvitationDTO(*inv, false))
}

// handleLeaveOrg → DELETE /api/v1/orgs/{id}/members/me. Any member may leave,
// subject to the same last-owner / last-admin guards that protect removal by an
// admin — leaving must not be a side door around them.
func (s *Server) handleLeaveOrg(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	u, isUser := auth.UserFromContext(r.Context())
	if !isUser {
		writeError(w, http.StatusForbidden, "human login required to leave an org")
		return
	}
	orgID := r.PathValue("id")
	email := strings.ToLower(strings.TrimSpace(u.Email))

	m, err := s.orgs.GetOrgMember(r.Context(), orgID, email)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not a member of this organization")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "cannot verify membership; try again")
		return
	}
	if !s.guardLastPrivileged(w, r, orgID, org.NormalizeRole(m.Role), "", "leave") {
		return
	}
	if err := s.orgs.RemoveOrgMember(r.Context(), orgID, email); err != nil {
		s.log.Error("admin: leave org", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to leave org")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

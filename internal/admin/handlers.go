package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	diskutil "github.com/shirou/gopsutil/v3/disk"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/org"
)

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the uniform {"error":"<msg>"} JSON envelope with status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

// tokenFromRequest extracts the raw JWT from the request: session cookie
// preferred (browser clients), then Authorization: Bearer (API/CLI clients).
func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(auth.TokenCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// ---- auth (public) -----------------------------------------------------------

// handleRegister → POST /api/v1/auth/register. Body: RegisterRequest.
// On success: sets the session cookie and returns LoginResponse. First account
// becomes admin (handled by auth.Service.Register).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	u, err := s.auth.Register(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			writeError(w, http.StatusConflict, "email already registered")
		case errors.Is(err, auth.ErrEmailRequired):
			writeError(w, http.StatusBadRequest, "email is required")
		case errors.Is(err, auth.ErrPasswordTooShort):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.log.Error("admin: register", "err", err)
			writeError(w, http.StatusInternalServerError, "registration failed")
		}
		return
	}

	token, err := s.tokens.Sign(*u)
	if err != nil {
		s.log.Error("admin: sign token after register", "err", err)
		writeError(w, http.StatusInternalServerError, "token issuance failed")
		return
	}

	auth.SetSessionCookie(w, token, s.secure)
	writeJSON(w, http.StatusOK, LoginResponse{User: toUserDTO(*u)})
}

// handleLogin → POST /api/v1/auth/login. Body: LoginRequest.
// On success: sets the session cookie and returns LoginResponse.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	token, u, err := s.auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		s.log.Error("admin: login", "err", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	auth.SetSessionCookie(w, token, s.secure)
	writeJSON(w, http.StatusOK, LoginResponse{User: toUserDTO(*u)})
}

// handleLogout → POST /api/v1/auth/logout. Reads the session, bumps token_gen
// (server-side revoke-all), and clears the cookie. Returns 204 on success.
// The route is public so it also works when the session is already expired.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := tokenFromRequest(r); token != "" {
		embedded, err := s.tokens.Verify(token)
		if err == nil {
			if logErr := s.auth.Logout(r.Context(), embedded.ID); logErr != nil {
				s.log.Error("admin: logout bump token_gen", "err", logErr, "user_id", embedded.ID)
			}
		}
	}
	auth.ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---- me (authenticated) ------------------------------------------------------

// handleMe → GET /api/v1/me. Returns MeResponse for the authenticated user,
// extended with org context when an org store is wired in.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	resp := MeResponse{
		User:    toUserDTO(u),
		IsAdmin: u.SystemRole == "admin",
	}

	if s.orgs != nil {
		// The active org is whatever PrincipalMiddleware actually resolved — a
		// real membership (or a system-role holder's implicit read-only view).
		// There is no DefaultOrgID fallback: telling a user who belongs to
		// nothing that they are in org_default is a phantom membership, and the
		// UI believed it and then 403'd on every org-scoped call.
		if active, ok := auth.ActiveOrgFromContext(r.Context()); ok {
			resp.ActiveOrgID = active.ID
			resp.ActiveOrgRole = active.Role
			resp.ActiveOrgSystemAccess = active.SystemAccess
		}

		orgs, err := s.orgs.ListOrgsForEmail(r.Context(), u.Email)
		if err != nil {
			s.log.Error("admin: list orgs for /me", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to resolve organizations")
			return
		}
		// Always a list, never null: "no orgs" is an answer the client must be
		// able to act on (show the invite-only + create-your-own state).
		resp.Orgs = make([]OrgDTO, 0, len(orgs))
		for _, o := range orgs {
			resp.Orgs = append(resp.Orgs, toOrgDTO(*o))
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---- stats (admin) -----------------------------------------------------------

// handleStats → GET /api/v1/admin/stats. Returns StatsResponse with per-protocol
// capacity rows, grand totals, and the blob backend's disk footprint.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	byProto, err := s.stats.ByProtocol(ctx)
	if err != nil {
		s.log.Error("admin: stats by protocol", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to retrieve stats")
		return
	}

	total, err := s.stats.Total(ctx)
	if err != nil {
		s.log.Error("admin: stats total", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to retrieve stats")
		return
	}

	var used int64
	if s.blobs != nil {
		used, _ = s.blobs.UsageBytes(ctx)
	}

	// Derive free bytes from the filesystem hosting the local blob root.
	// S3 and other drivers do not have a meaningful filesystem free value.
	var free int64
	if s.cfg != nil && s.cfg.Storage.Blob.Driver == "local" && s.cfg.Storage.Blob.Local.Root != "" {
		if du, duErr := diskutil.Usage(s.cfg.Storage.Blob.Local.Root); duErr == nil {
			free = int64(du.Free)
		}
	}

	rows := make([]ProtocolStat, 0, len(byProto))
	for proto, ss := range byProto {
		ps := ProtocolStat{
			Protocol: proto,
			Bytes:    ss.Bytes,
			Objects:  ss.Objects,
		}
		if !ss.Oldest.IsZero() {
			ps.OldestUnix = ss.Oldest.Unix()
		}
		if !ss.Newest.IsZero() {
			ps.NewestUnix = ss.Newest.Unix()
		}
		rows = append(rows, ps)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Protocol < rows[j].Protocol })

	writeJSON(w, http.StatusOK, StatsResponse{
		PerProtocol:     rows,
		TotalBytes:      total.Bytes,
		TotalObjects:    total.Objects,
		BackendDiskFree: free,
		BackendDiskUsed: used,
	})
}

// handleStatsSeries → GET /api/v1/admin/stats/series?protocol=<p>.
// Returns SeriesResponse (grand total when protocol is omitted).
//
// NOTE: stats.Collector does not yet expose a time-series ring buffer; a
// single-point snapshot of the current aggregate is returned as a
// best-effort fallback until a Series(ctx, protocol) method is added to
// the Collector interface (missing dep — tracked as TODO).
func (s *Server) handleStatsSeries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	proto := r.URL.Query().Get("protocol")

	var bytes int64
	if proto == "" {
		total, err := s.stats.Total(ctx)
		if err != nil {
			s.log.Error("admin: stats series total", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to retrieve stats")
			return
		}
		bytes = total.Bytes
	} else {
		byProto, err := s.stats.ByProtocol(ctx)
		if err != nil {
			s.log.Error("admin: stats series by protocol", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to retrieve stats")
			return
		}
		if ss, ok := byProto[proto]; ok {
			bytes = ss.Bytes
		}
	}

	writeJSON(w, http.StatusOK, SeriesResponse{
		Protocol: proto,
		Points:   []SeriesPoint{{Unix: time.Now().Unix(), Bytes: bytes}},
	})
}

// handleUpstreams now lives in upstreams.go, backed by the live per-protocol
// upstream Runtime registry (Deps.Upstreams) rather than a config echo.

// ---- users (admin) -----------------------------------------------------------

// handleListUsers → GET /api/v1/admin/users?limit=&offset=. Returns UsersResponse.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	users, total, err := s.users.ListUsers(r.Context(), limit, offset)
	if err != nil {
		s.log.Error("admin: list users", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	dtos := make([]UserDTO, 0, len(users))
	for _, u := range users {
		dtos = append(dtos, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, UsersResponse{Users: dtos, Total: total})
}

// handleCreateUser → POST /api/v1/admin/users. Body: CreateUserRequest.
// Returns 201 + UserDTO. Admin creates the account directly; system_role
// defaults to "user" when omitted. This path does NOT apply first-user-admin
// logic (that is only for self-registration via /auth/register).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if len(req.Password) < auth.MinPasswordLen {
		writeError(w, http.StatusBadRequest, auth.ErrPasswordTooShort.Error())
		return
	}

	role := req.SystemRole
	if role == "" {
		role = "user"
	}

	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		s.log.Error("admin: create user hash password", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	u, err := s.users.CreateUser(r.Context(), auth.User{
		Email:        email,
		Name:         req.Name,
		PasswordHash: hash,
		SystemRole:   role,
	})
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			writeError(w, http.StatusConflict, "email already registered")
			return
		}
		s.log.Error("admin: create user", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	writeJSON(w, http.StatusCreated, toUserDTO(*u))
}

// handleGetUser → GET /api/v1/admin/users/{id}. Returns UserDTO.
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}

	u, err := s.users.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		s.log.Error("admin: get user", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(*u))
}

// handlePatchUser → PATCH /api/v1/admin/users/{id}. Body: PatchUserRequest.
// Returns UserDTO.
//
// Role changes are handled via auth.UserStore.UpdateUserRole.
// Name and password changes are handled via auth.UserStore.UpdateUserFields.
// Last-admin and self-modification guards are enforced before any mutation.
func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}

	var req PatchUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Nothing to do.
	if req.Name == nil && req.SystemRole == nil && req.Password == nil {
		u, err := s.users.GetUserByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, auth.ErrUserNotFound) {
				writeError(w, http.StatusNotFound, "user not found")
				return
			}
			s.log.Error("admin: patch user get (noop)", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "failed to get user")
			return
		}
		writeJSON(w, http.StatusOK, toUserDTO(*u))
		return
	}

	ctx := r.Context()

	current, err := s.users.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		s.log.Error("admin: patch user get", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}

	// Role change with last-admin guard.
	if req.SystemRole != nil && *req.SystemRole != current.SystemRole {
		newRole := *req.SystemRole
		if newRole != "admin" && newRole != "user" {
			writeError(w, http.StatusBadRequest, "system_role must be 'admin' or 'user'")
			return
		}
		// Guard: cannot demote the last admin.
		if current.SystemRole == "admin" && newRole != "admin" {
			isLast, checkErr := s.isLastAdmin(ctx, id)
			if checkErr != nil {
				s.log.Error("admin: patch user last-admin check", "err", checkErr)
				writeError(w, http.StatusInternalServerError, "failed to verify admin count")
				return
			}
			if isLast {
				writeError(w, http.StatusConflict, "cannot demote the last admin")
				return
			}
		}
		if roleErr := s.users.UpdateUserRole(ctx, id, newRole); roleErr != nil {
			s.log.Error("admin: patch user role", "err", roleErr, "id", id)
			writeError(w, http.StatusInternalServerError, "failed to update user role")
			return
		}
	}

	// Name / password changes: call UpdateUserFields directly (now part of
	// auth.UserStore so all concrete stores implement it).
	if req.Name != nil || req.Password != nil {
		var hashPtr *string
		if req.Password != nil {
			h, hashErr := s.hasher.Hash(*req.Password)
			if hashErr != nil {
				s.log.Error("admin: patch user hash password", "err", hashErr)
				writeError(w, http.StatusInternalServerError, "failed to update password")
				return
			}
			hashPtr = &h
		}
		if updErr := s.users.UpdateUserFields(ctx, id, req.Name, hashPtr); updErr != nil {
			if errors.Is(updErr, auth.ErrUserNotFound) {
				writeError(w, http.StatusNotFound, "user not found")
				return
			}
			s.log.Error("admin: patch user fields", "err", updErr, "id", id)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}

	// Re-fetch the authoritative post-update state.
	updated, err := s.users.GetUserByID(ctx, id)
	if err != nil {
		// Extremely unlikely (just updated it); return what we started with.
		s.log.Warn("admin: patch user re-fetch failed", "err", err, "id", id)
		writeJSON(w, http.StatusOK, toUserDTO(*current))
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(*updated))
}

// handleDeleteUser → DELETE /api/v1/admin/users/{id}. Returns 204.
// Guards: (a) cannot delete self, (b) cannot delete the last admin.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	// Self-delete guard.
	if caller, callerOK := auth.UserFromContext(ctx); callerOK && caller.ID == id {
		writeError(w, http.StatusConflict, "cannot delete your own account")
		return
	}

	target, err := s.users.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		s.log.Error("admin: delete user get", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}

	// Last-admin guard.
	if target.SystemRole == "admin" {
		isLast, checkErr := s.isLastAdmin(ctx, id)
		if checkErr != nil {
			s.log.Error("admin: delete user last-admin check", "err", checkErr)
			writeError(w, http.StatusInternalServerError, "failed to verify admin count")
			return
		}
		if isLast {
			writeError(w, http.StatusConflict, "cannot delete the last admin")
			return
		}
	}

	if err := s.users.DeleteUser(ctx, id); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		s.log.Error("admin: delete user", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- config + events (admin) -------------------------------------------------

// handleConfig → GET /api/v1/admin/config. Returns ConfigResponse (redacted).
// Secrets (jwt_secret, admin_key, DSN passwords, S3 credentials) are never
// included in the response.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeError(w, http.StatusInternalServerError, "configuration not available")
		return
	}

	resp := ConfigResponse{
		DataPlaneAddr:    s.cfg.Server.DataPlaneAddr,
		ControlPlaneAddr: s.cfg.Server.ControlPlaneAddr,
		BlobDriver:       s.cfg.Storage.Blob.Driver,
		MetaDriver:       s.cfg.Storage.Meta.Driver,
		Protocols:        make([]ProtocolConfigView, 0, len(s.cfg.Protocols)),
	}

	for proto, pc := range s.cfg.Protocols {
		pv := ProtocolConfigView{
			Protocol:      proto,
			MutableTTLSec: pc.MutableTTLSeconds,
			VerifyTiers:   pc.Verification.Tiers,
			Upstreams:     make([]UpstreamView, 0, len(pc.Upstreams)),
		}
		if pv.VerifyTiers == nil {
			pv.VerifyTiers = []string{}
		}
		for _, u := range pc.Upstreams {
			pv.Upstreams = append(pv.Upstreams, UpstreamView{
				Name:     u.Name,
				BaseURL:  u.BaseURL,
				Priority: u.Priority,
				Official: u.Official,
			})
		}
		resp.Protocols = append(resp.Protocols, pv)
	}
	sort.Slice(resp.Protocols, func(i, j int) bool {
		return resp.Protocols[i].Protocol < resp.Protocols[j].Protocol
	})

	writeJSON(w, http.StatusOK, resp)
}

// handleEvents → GET /api/v1/admin/events?limit=. Returns EventsResponse.
//
// NOTE: no EventStore has been added to Deps yet; the verification event
// persistence layer is a future addition. An empty list is returned until
// an EventStore interface and implementation are wired in (missing dep —
// tracked as TODO).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, EventsResponse{Events: []VerificationEvent{}})
}

// ---- internal helpers --------------------------------------------------------

// parseUserID extracts and validates the {id} path value from r.
func parseUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return 0, false
	}
	return id, true
}

// isLastAdmin reports whether the user identified by id is the only remaining
// admin in the store. It is only called when the caller already knows the
// target user is an admin.
func (s *Server) isLastAdmin(ctx context.Context, id int64) (bool, error) {
	users, _, err := s.users.ListUsers(ctx, 0, 0)
	if err != nil {
		return false, err
	}
	adminCount := 0
	for _, u := range users {
		if u.SystemRole == "admin" {
			adminCount++
		}
	}
	// If there is only one admin, that must be the user we were called with.
	return adminCount <= 1, nil
}

// ---- DTO converters ----------------------------------------------------------

// toOrgDTO converts an org.Org to its client-facing projection.
func toOrgDTO(o org.Org) OrgDTO {
	return OrgDTO{
		ID:           o.ID,
		Name:         o.Name,
		Slug:         o.Slug,
		Status:       o.Status,
		CreatedBy:    o.CreatedBy,
		CreatedAt:    o.CreatedAt,
		Role:         o.Role,
		SystemAccess: o.SystemAccess,
	}
}

// toMemberDTO converts an org.Member to its client-facing projection.
func toMemberDTO(m org.Member) MemberDTO {
	return MemberDTO{
		ID:        m.ID,
		OrgID:     m.OrgID,
		Email:     m.Email,
		Role:      m.Role,
		InvitedBy: m.InvitedBy,
		CreatedAt: m.CreatedAt,
	}
}

// toInvitationDTO converts an org.Invitation to its client-facing projection.
// withToken gates the one-time disclosure of the invitation token: the creation
// response carries it (it is how the invitee is reached, standing in for email
// delivery), every other view withholds it.
func toInvitationDTO(inv org.Invitation, withToken bool) InvitationDTO {
	dto := InvitationDTO{
		ID:        inv.ID,
		OrgID:     inv.OrgID,
		Email:     inv.Email,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		Status:    inv.Status,
		ExpiresAt: inv.ExpiresAt,
		CreatedAt: inv.CreatedAt,
	}
	if withToken {
		dto.Token = inv.Token
	}
	return dto
}

// decodeJSONBody decodes an request body, writing the uniform 400 on failure.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return err
	}
	return nil
}

// slugify renders a display name as a lower-case hyphenated slug, so callers
// creating an org need only supply a name (mirrors ai-sandbox).
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// callerIsOrgOwner reports whether the caller wields owner-level authority over
// orgID. Ownership-sensitive acts (granting owner, demoting or removing a
// sitting owner) demand it: plain org admins manage members, but they must not
// be able to seize or dissolve ownership. System admins pass as platform
// superusers.
func (s *Server) callerIsOrgOwner(r *http.Request, orgID string) bool {
	if subj, _ := auth.SubjectFromContext(r.Context()); subj.Admin {
		return true
	}
	u, isUser := auth.UserFromContext(r.Context())
	if !isUser || s.orgs == nil {
		return false
	}
	m, err := s.orgs.GetOrgMember(r.Context(), orgID, strings.ToLower(strings.TrimSpace(u.Email)))
	return err == nil && org.NormalizeRole(m.Role) == org.RoleOwner
}

// guardLastPrivileged blocks changes that would strand an org without an owner
// (or without an admin). curRole is the member's role today; newRole is what
// they are becoming ("" for removal/leave). action names the act for the error
// message ("demote" / "remove" / "leave").
//
// fail-CLOSED: if a count cannot be read, the change is refused with 503 rather
// than waved through. A guard that disables itself under database trouble is
// exactly when the last owner gets removed.
func (s *Server) guardLastPrivileged(w http.ResponseWriter, r *http.Request, orgID, curRole, newRole, action string) bool {
	if curRole == org.RoleOwner && !org.AtLeast(newRole, org.RoleOwner) {
		n, err := s.orgs.CountOrgOwners(r.Context(), orgID)
		if err != nil {
			s.log.Error("admin: count org owners", "err", err, "org_id", orgID)
			writeError(w, http.StatusServiceUnavailable, "cannot verify owner count; try again")
			return false
		}
		if n <= 1 {
			writeError(w, http.StatusConflict, "cannot "+action+" the last owner of the organization")
			return false
		}
	}
	// Owners are counted on the ownership axis above, not here: CountOrgAdmins
	// tallies role="admin" only, so running this branch for an owner would
	// spuriously 409 on an org that has owners but no separate admin.
	if curRole != org.RoleOwner && org.AtLeast(curRole, org.RoleAdmin) && !org.AtLeast(newRole, org.RoleAdmin) {
		n, err := s.orgs.CountOrgAdmins(r.Context(), orgID)
		if err != nil {
			s.log.Error("admin: count org admins", "err", err, "org_id", orgID)
			writeError(w, http.StatusServiceUnavailable, "cannot verify admin count; try again")
			return false
		}
		if n <= 1 {
			writeError(w, http.StatusConflict, "cannot "+action+" the last admin of the organization")
			return false
		}
	}
	return true
}

// toKeyDTO converts an apikey.KeyInfo to its client-facing projection.
func toKeyDTO(k apikey.KeyInfo, rawKey string) KeyDTO {
	return KeyDTO{
		ID:         k.ID,
		OrgID:      k.OrgID,
		Label:      k.Label,
		Prefix:     k.Prefix,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		Revoked:    k.Revoked,
		RawKey:     rawKey,
	}
}

// ---- api keys ----------------------------------------------------------------

// handleCreateKey → POST /api/v1/keys.
// Creates an API key for the active org of the caller. The raw plaintext key
// is returned exactly once in the response body.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeError(w, http.StatusNotImplemented, "key store not configured")
		return
	}

	var req CreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	orgID, _ := auth.OrgFromContext(r.Context())
	if orgID == "" {
		orgID = apikey.DefaultOrgID
	}

	subj, _ := auth.SubjectFromContext(r.Context())
	var id, rawKey string
	var err error

	if subj.UserID != "" {
		id, rawKey, err = s.keys.CreateOwned(orgID, subj.UserID, req.Label)
	} else {
		id, rawKey, err = s.keys.Create(orgID, req.Label)
	}
	if err != nil {
		s.log.Error("admin: create key", "err", err)
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

// handleListKeys → GET /api/v1/keys.
// Returns all keys for the active org.
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeError(w, http.StatusNotImplemented, "key store not configured")
		return
	}

	orgID, _ := auth.OrgFromContext(r.Context())
	if orgID == "" {
		orgID = apikey.DefaultOrgID
	}

	infos, err := s.keys.List(orgID)
	if err != nil {
		s.log.Error("admin: list keys", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	dtos := make([]KeyDTO, 0, len(infos))
	for _, k := range infos {
		dtos = append(dtos, toKeyDTO(k, ""))
	}
	writeJSON(w, http.StatusOK, KeysResponse{Keys: dtos})
}

// handleRevokeKey → DELETE /api/v1/keys/{id}.
// Soft-deletes a key; the key is rejected immediately on subsequent lookups.
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeError(w, http.StatusNotImplemented, "key store not configured")
		return
	}

	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key id required")
		return
	}

	orgID, _ := auth.OrgFromContext(r.Context())
	if orgID == "" {
		orgID = apikey.DefaultOrgID
	}

	found, err := s.keys.Revoke(orgID, keyID)
	if err != nil {
		s.log.Error("admin: revoke key", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- orgs --------------------------------------------------------------------

// handleListOrgs → GET /api/v1/orgs.
// For session users: returns the orgs they belong to. For system admins: also
// returns all orgs (same as ListOrgs). For API key callers: returns the key's
// pinned org only.
func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}

	u, isUser := auth.UserFromContext(r.Context())
	subj, _ := auth.SubjectFromContext(r.Context())

	var orgList []*org.Org
	var err error

	if isUser {
		orgList, err = s.orgs.ListOrgsForEmail(r.Context(), u.Email)
		if err != nil {
			s.log.Error("admin: list orgs for email", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to list orgs")
			return
		}
		// A system-role holder additionally sees every other org as an implicit
		// read-only viewer, marked system_access so the UI can distinguish a
		// real membership from a backoffice view. Membership always wins.
		if org.NormalizeSystemRole(u.SystemRole) != "" {
			member := make(map[string]bool, len(orgList))
			for _, o := range orgList {
				member[o.ID] = true
			}
			all, listErr := s.orgs.ListOrgs(r.Context())
			if listErr != nil {
				s.log.Error("admin: list all orgs", "err", listErr)
				writeError(w, http.StatusInternalServerError, "failed to list orgs")
				return
			}
			for _, o := range all {
				if member[o.ID] {
					continue
				}
				o.Role = org.RoleViewer
				o.SystemAccess = true
				orgList = append(orgList, o)
			}
		}
	} else if subj.OrgID != "" {
		// API-key caller: return just the key's pinned org.
		o, getErr := s.orgs.GetOrg(r.Context(), subj.OrgID)
		if getErr == nil {
			orgList = []*org.Org{o}
		}
	}

	dtos := make([]OrgDTO, 0, len(orgList))
	for _, o := range orgList {
		dtos = append(dtos, toOrgDTO(*o))
	}
	writeJSON(w, http.StatusOK, OrgsResponse{Orgs: dtos})
}

// handleCreateOrg → POST /api/v1/orgs.
// Creates a new org; the caller becomes the org owner.
func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}

	var req CreateOrgRequest
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

	// Self-service org creation is for humans only: the new org's owner is the
	// caller's email, and an API key has no email to own anything with. This is
	// also the escape hatch for a user who belongs to no org, so it must not be
	// gated on already having one.
	u, isUser := auth.UserFromContext(r.Context())
	if !isUser {
		writeError(w, http.StatusForbidden, "human login required to create an org")
		return
	}

	subj, _ := auth.SubjectFromContext(r.Context())

	// Self-service org quota (settings.KeyOrgMaxPerUser; default 1, 0 =
	// unlimited). Hot-reloadable: the effective value is read per request, so an
	// admin raising the limit takes effect on the very next create with no
	// restart. System admins are exempt — they are the ones who set the limit,
	// and locking an operator out of creating an org is the failure mode this
	// endpoint exists to prevent.
	//
	// Counted on created_by, not membership: being invited into other people's
	// orgs must never consume your own allowance.
	if err := s.checkOrgQuota(r.Context(), subj.UserID, u.SystemRole); err != nil {
		if errors.Is(err, errOrgQuotaExceeded) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.log.Error("admin: org quota check", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to check org quota")
		return
	}

	now := time.Now().UTC()
	newOrg := &org.Org{
		Name:      name,
		Slug:      slug,
		Status:    org.StatusActive,
		CreatedBy: subj.UserID,
		CreatedAt: now,
	}

	if err := s.orgs.CreateOrg(r.Context(), newOrg); err != nil {
		s.log.Error("admin: create org", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create org")
		return
	}

	// The creator becomes owner. This is not best-effort: an org whose creator
	// is not a member is an org nobody can administer — the exact dead end this
	// endpoint exists to escape.
	if err := s.orgs.AddOrgMember(r.Context(), &org.Member{
		OrgID:     newOrg.ID,
		Email:     u.Email,
		Role:      org.RoleOwner,
		CreatedAt: now,
	}); err != nil {
		s.log.Error("admin: add creator as owner", "err", err, "org_id", newOrg.ID)
		writeError(w, http.StatusInternalServerError, "failed to assign org owner")
		return
	}

	newOrg.Role = org.RoleOwner
	writeJSON(w, http.StatusCreated, toOrgDTO(*newOrg))
}

// handleGetOrg → GET /api/v1/orgs/{id}.
// Returns an org the caller is a member of (or any org for system admins).
func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}

	orgID := r.PathValue("id")
	if orgID == "" {
		writeError(w, http.StatusBadRequest, "org id required")
		return
	}

	o, err := s.orgs.GetOrg(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		s.log.Error("admin: get org", "err", err, "id", orgID)
		writeError(w, http.StatusInternalServerError, "failed to get org")
		return
	}

	// Check membership (system admins bypass).
	subj, _ := auth.SubjectFromContext(r.Context())
	if !subj.Admin {
		u, isUser := auth.UserFromContext(r.Context())
		if isUser {
			_, memErr := s.orgs.GetOrgMember(r.Context(), orgID, u.Email)
			if memErr != nil {
				writeError(w, http.StatusForbidden, "not a member of this org")
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, toOrgDTO(*o))
}

// handleUpdateOrg → PUT /api/v1/orgs/{id}. Updates the org's display name.
// Requires org admin on the path org (or system editor+).
func (s *Server) handleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	o, err := s.orgs.GetOrg(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get org")
		return
	}
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	var req UpdateOrgRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		o.Name = name
	}
	if err := s.orgs.UpdateOrg(r.Context(), o); err != nil {
		s.log.Error("admin: update org", "err", err, "org_id", orgID)
		writeError(w, http.StatusInternalServerError, "failed to update org")
		return
	}
	writeJSON(w, http.StatusOK, toOrgDTO(*o))
}

// ---- members -----------------------------------------------------------------

// requireOrgAdmin checks that the caller holds at least the admin role in the
// given org. Returns true on success, writes the error response and returns
// false on failure.
// Authorization is decided against the PATH org, never the caller's active org:
// holding admin in one org must not confer member management in another.
func (s *Server) requireOrgAdmin(w http.ResponseWriter, r *http.Request, orgID string) bool {
	subj, _ := auth.SubjectFromContext(r.Context())
	if subj.Admin {
		return true // system admin bypass
	}
	if s.orgs == nil {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	// An API key is org-admin inside the org it is pinned to, and nowhere else.
	u, isUser := auth.UserFromContext(r.Context())
	if !isUser {
		if subj.UserID != "" && subj.OrgID == orgID {
			return true
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	// System editor+ manages members from the backoffice, across orgs.
	if org.AtLeast(org.NormalizeSystemRole(u.SystemRole), org.RoleEditor) {
		return true
	}
	mem, err := s.orgs.GetOrgMember(r.Context(), orgID, u.Email)
	if err != nil {
		writeError(w, http.StatusForbidden, "not a member of this org")
		return false
	}
	if !org.AtLeast(org.NormalizeRole(mem.Role), org.RoleAdmin) {
		writeError(w, http.StatusForbidden, "org admin role required")
		return false
	}
	return true
}

// handleListMembers → GET /api/v1/orgs/{id}/members. Returns MembersResponse.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	members, err := s.orgs.ListOrgMembers(r.Context(), orgID)
	if err != nil {
		s.log.Error("admin: list org members", "err", err, "org_id", orgID)
		writeError(w, http.StatusInternalServerError, "failed to list members")
		return
	}
	dtos := make([]MemberDTO, 0, len(members))
	for _, m := range members {
		dtos = append(dtos, toMemberDTO(*m))
	}
	writeJSON(w, http.StatusOK, MembersResponse{Members: dtos})
}

// handleAddMember → POST /api/v1/orgs/{id}/members. Returns 201 + MemberDTO.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	var req AddMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	role := org.RoleViewer // least privilege: promoting is an explicit act
	if strings.TrimSpace(req.Role) != "" {
		if role = org.NormalizeLegacyRole(req.Role); role == "" {
			writeError(w, http.StatusBadRequest, "role must be viewer, editor, admin or owner")
			return
		}
	}

	// Current role decides both the ownership check and the guards below.
	// fail-CLOSED: only a genuine "not a member yet" may read as no role — a
	// lookup failure must not quietly switch the guards off.
	curRole := ""
	if cur, err := s.orgs.GetOrgMember(r.Context(), orgID, req.Email); err == nil {
		curRole = org.NormalizeRole(cur.Role)
	} else if !errors.Is(err, org.ErrNotFound) {
		writeError(w, http.StatusServiceUnavailable, "cannot verify current membership; try again")
		return
	}

	// Ownership-sensitive: granting owner, or touching a sitting owner, requires
	// the caller to be an owner themselves. Member management is admin+, but
	// ownership transfer is owner-only.
	if role == org.RoleOwner || curRole == org.RoleOwner {
		if !s.callerIsOrgOwner(r, orgID) {
			writeError(w, http.StatusForbidden, "org owner role required for ownership changes")
			return
		}
	}
	if curRole != "" && !s.guardLastPrivileged(w, r, orgID, curRole, role, "demote") {
		return
	}

	subj, _ := auth.SubjectFromContext(r.Context())
	m := &org.Member{
		OrgID:     orgID,
		Email:     req.Email,
		Role:      role,
		InvitedBy: subj.UserID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.orgs.AddOrgMember(r.Context(), m); err != nil {
		s.log.Error("admin: add org member", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to add member")
		return
	}
	// Re-fetch to get server-generated ID.
	added, err := s.orgs.GetOrgMember(r.Context(), orgID, req.Email)
	if err != nil {
		writeJSON(w, http.StatusCreated, toMemberDTO(*m))
		return
	}
	writeJSON(w, http.StatusCreated, toMemberDTO(*added))
}

// handlePatchMember → PATCH /api/v1/orgs/{id}/members/{email}. Returns 200 + MemberDTO.
func (s *Server) handlePatchMember(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	email := strings.ToLower(r.PathValue("email"))
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	var req PatchMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Role == nil {
		// Nothing to update; return current state.
		m, err := s.orgs.GetOrgMember(r.Context(), orgID, email)
		if err != nil {
			if errors.Is(err, org.ErrNotFound) {
				writeError(w, http.StatusNotFound, "member not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to get member")
			return
		}
		writeJSON(w, http.StatusOK, toMemberDTO(*m))
		return
	}

	current, err := s.orgs.GetOrgMember(r.Context(), orgID, email)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "member not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get member")
		return
	}

	newRole := org.NormalizeLegacyRole(*req.Role)
	if newRole == "" {
		writeError(w, http.StatusBadRequest, "role must be viewer, editor, admin or owner")
		return
	}
	curRole := org.NormalizeRole(current.Role)

	// Ownership-sensitive: granting owner or changing a sitting owner is
	// owner-only, even for an org admin.
	if newRole == org.RoleOwner || curRole == org.RoleOwner {
		if !s.callerIsOrgOwner(r, orgID) {
			writeError(w, http.StatusForbidden, "org owner role required for ownership changes")
			return
		}
	}
	if !s.guardLastPrivileged(w, r, orgID, curRole, newRole, "demote") {
		return
	}

	current.Role = newRole
	if err := s.orgs.AddOrgMember(r.Context(), current); err != nil {
		s.log.Error("admin: patch member role", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update member role")
		return
	}
	updated, err := s.orgs.GetOrgMember(r.Context(), orgID, email)
	if err != nil {
		writeJSON(w, http.StatusOK, toMemberDTO(*current))
		return
	}
	writeJSON(w, http.StatusOK, toMemberDTO(*updated))
}

// handleRemoveMember → DELETE /api/v1/orgs/{id}/members/{email}. Returns 204.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	if s.orgs == nil {
		writeError(w, http.StatusNotImplemented, "org store not configured")
		return
	}
	orgID := r.PathValue("id")
	email := strings.ToLower(r.PathValue("email"))
	if !s.requireOrgAdmin(w, r, orgID) {
		return
	}

	target, err := s.orgs.GetOrgMember(r.Context(), orgID, email)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			writeError(w, http.StatusNotFound, "member not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get member")
		return
	}

	curRole := org.NormalizeRole(target.Role)
	// Ownership-sensitive: removing a sitting owner is owner-only.
	if curRole == org.RoleOwner && !s.callerIsOrgOwner(r, orgID) {
		writeError(w, http.StatusForbidden, "org owner role required to remove an owner")
		return
	}
	if !s.guardLastPrivileged(w, r, orgID, curRole, "", "remove") {
		return
	}

	if err := s.orgs.RemoveOrgMember(r.Context(), orgID, email); err != nil {
		s.log.Error("admin: remove member", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to remove member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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

	"github.com/ivanzzeth/specula/internal/auth"
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

// handleMe → GET /api/v1/me. Returns MeResponse for the authenticated user.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, MeResponse{User: toUserDTO(u)})
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

// handleUpstreams → GET /api/v1/admin/upstreams. Returns UpstreamsResponse.
//
// NOTE: the Deps struct does not include an upstream block tracker, so live
// circuit-breaker state (Blocked, LastErr) is not available here. Upstreams
// are derived from config with Blocked=false and LastErr="" as a best-effort
// snapshot. Wiring a UpstreamTracker dep is tracked as TODO.
func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	var upstreams []UpstreamHealth
	if s.cfg != nil {
		for proto, pc := range s.cfg.Protocols {
			for _, u := range pc.Upstreams {
				upstreams = append(upstreams, UpstreamHealth{
					Protocol: proto,
					URL:      u.BaseURL,
					Blocked:  false,
					LastErr:  "",
				})
			}
		}
		// Sort for deterministic output.
		sort.Slice(upstreams, func(i, j int) bool {
			if upstreams[i].Protocol != upstreams[j].Protocol {
				return upstreams[i].Protocol < upstreams[j].Protocol
			}
			return upstreams[i].URL < upstreams[j].URL
		})
	}
	if upstreams == nil {
		upstreams = []UpstreamHealth{}
	}
	writeJSON(w, http.StatusOK, UpstreamsResponse{Upstreams: upstreams})
}

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

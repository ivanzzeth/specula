package admin

import (
	"net/http"

	"github.com/ivanzzeth/specula/internal/auth"
)

// RegisterRoutes mounts the /api/v1 control-plane API onto mux using Go 1.22
// method+pattern routing. Three access tiers are applied:
//
//	public     — no session required (register / login / logout)
//	authed     — any logged-in user (auth.Middleware)          → GET /api/v1/me
//	adminOnly  — logged-in AND system_role=="admin"            → /api/v1/admin/*
//
// The auth middleware is built from the same TokenVerifier + UserStore the
// server was constructed with, so revocation (token_gen bump on logout) is
// enforced on every authenticated request.
//
// Handlers are currently 501 stubs (see handlers.go); the routing table, method
// constraints, and middleware wrapping are the stable contract other agents
// build against.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	authMW := auth.Middleware(s.tokens, s.users)
	authed := func(h http.HandlerFunc) http.Handler { return authMW(h) }
	adminOnly := func(h http.HandlerFunc) http.Handler { return authMW(auth.AdminRequired(h)) }

	// ── public: no authentication ────────────────────────────────────────────
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// ── authenticated: any logged-in user ────────────────────────────────────
	mux.Handle("GET /api/v1/me", authed(s.handleMe))

	// ── admin-only: system_role=="admin" ─────────────────────────────────────
	mux.Handle("GET /api/v1/admin/stats", adminOnly(s.handleStats))
	mux.Handle("GET /api/v1/admin/stats/series", adminOnly(s.handleStatsSeries))
	mux.Handle("GET /api/v1/admin/upstreams", adminOnly(s.handleUpstreams))

	mux.Handle("GET /api/v1/admin/users", adminOnly(s.handleListUsers))
	mux.Handle("POST /api/v1/admin/users", adminOnly(s.handleCreateUser))
	mux.Handle("GET /api/v1/admin/users/{id}", adminOnly(s.handleGetUser))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminOnly(s.handlePatchUser))
	mux.Handle("DELETE /api/v1/admin/users/{id}", adminOnly(s.handleDeleteUser))

	mux.Handle("GET /api/v1/admin/config", adminOnly(s.handleConfig))
	mux.Handle("GET /api/v1/admin/events", adminOnly(s.handleEvents))
}

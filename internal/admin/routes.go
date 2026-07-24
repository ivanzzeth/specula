package admin

import (
	"net/http"

	"github.com/ivanzzeth/specula/internal/auth"
)

// RegisterRoutes mounts the /api/v1 control-plane API onto mux using Go 1.22
// method+pattern routing. Four access tiers are applied:
//
//	public        — no session required (register / login / logout)
//	authed        — any logged-in user (auth.Middleware)          → GET /api/v1/me
//	adminOnly     — logged-in AND system_role=="admin"            → /api/v1/admin/*
//	authedMT      — any authenticated principal (session JWT or API key)
//	               uses PrincipalMiddleware for multi-tenant routes
//
// The auth middleware is built from the same TokenVerifier + UserStore the
// server was constructed with, so revocation (token_gen bump on logout) is
// enforced on every authenticated request.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	authMW := auth.Middleware(s.tokens, s.users)
	adminOnly := func(h http.HandlerFunc) http.Handler { return authMW(auth.AdminRequired(h)) }

	// ── public: no authentication ────────────────────────────────────────────
	// Public: the browser needs the registry address before/without a session.
	mux.HandleFunc("GET /api/v1/instance", s.handleInstance)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// ── admin-only: system_role=="admin" ─────────────────────────────────────
	mux.Handle("GET /api/v1/admin/stats", adminOnly(s.handleStats))
	mux.Handle("GET /api/v1/admin/stats/series", adminOnly(s.handleStatsSeries))

	// Upstream mirror chains (R3 §5.3): live per-protocol health/latency/serve
	// share, plus runtime steering (reorder / enable-disable / unblock). The
	// mutations are admin-only because they change what every tenant's cache
	// misses hit.
	mux.Handle("GET /api/v1/admin/upstreams", adminOnly(s.handleUpstreams))
	mux.Handle("POST /api/v1/admin/upstreams/{protocol}/reorder", adminOnly(s.handleReorderUpstreams))
	mux.Handle("PATCH /api/v1/admin/upstreams/{protocol}/{id}", adminOnly(s.handlePatchUpstream))
	mux.Handle("POST /api/v1/admin/upstreams/{protocol}/{id}/unblock", adminOnly(s.handleUnblockUpstream))
	mux.Handle("POST /api/v1/admin/upstreams/{protocol}/{id}/probe", adminOnly(s.handleProbeUpstream))

	// Cache browser (R3 §5.2): per-protocol paginated listing of what is
	// actually cached, plus per-entry eviction and pin/protect. Admin-only: the
	// cache is a shared, cross-tenant resource.
	mux.Handle("GET /api/v1/admin/cache/{protocol}", adminOnly(s.handleListCache))
	mux.Handle("DELETE /api/v1/admin/cache/{protocol}/{id}", adminOnly(s.handleDeleteCacheEntry))
	mux.Handle("POST /api/v1/admin/cache/{protocol}/{id}/pin", adminOnly(s.handlePinCacheEntry))

	mux.Handle("GET /api/v1/admin/users", adminOnly(s.handleListUsers))
	mux.Handle("POST /api/v1/admin/users", adminOnly(s.handleCreateUser))
	mux.Handle("GET /api/v1/admin/users/{id}", adminOnly(s.handleGetUser))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminOnly(s.handlePatchUser))
	mux.Handle("DELETE /api/v1/admin/users/{id}", adminOnly(s.handleDeleteUser))

	mux.Handle("GET /api/v1/admin/config", adminOnly(s.handleConfig))
	mux.Handle("GET /api/v1/admin/events", adminOnly(s.handleEvents))
	mux.Handle("GET /api/v1/admin/events/series", adminOnly(s.handleEventsSeries))

	// Runtime settings (ported settings layer): the writable counterpart to the
	// read-only /admin/config echo above. GET lists every known setting with its
	// effective source (secrets redacted); PUT/DELETE write/clear the encrypted
	// runtime override and fire that key's reload hook. Admin-only: these knobs
	// include the session signing secret and the registry token key.
	mux.Handle("GET /api/v1/admin/settings", adminOnly(s.handleListSettings))
	mux.Handle("PUT /api/v1/admin/settings/{key}", adminOnly(s.handlePutSetting))
	mux.Handle("DELETE /api/v1/admin/settings/{key}", adminOnly(s.handleDeleteSetting))

	// ── multi-tenant: PrincipalMiddleware (JWT or API key) ───────────────────
	// principalMW resolves acl.Subject + active org and injects both into ctx.
	principalMW := auth.PrincipalMiddleware(s.keys, s.orgs, s.tokens, s.users)

	// authedMT requires any non-anonymous subject (apikey or session user).
	requireSubject := func(h http.HandlerFunc) http.Handler {
		return principalMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subj, _ := auth.SubjectFromContext(r.Context())
			if subj.UserID == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}))
	}

	// /me is mounted on principalMW, not the plain session middleware: it must
	// report the SAME active org the org-scoped routes will actually enforce.
	// Resolving it independently is what let /me claim org_default while every
	// other endpoint disagreed.
	mux.Handle("GET /api/v1/me", requireSubject(s.handleMe))

	// Instance stats for CLI (cache + traffic). Any API key or session works;
	// admin-only /admin/stats remains the WebUI capacity endpoint.
	mux.Handle("GET /api/v1/stats", requireSubject(s.handleInstanceStats))

	// Keys (org-scoped, any authenticated principal).
	mux.Handle("POST /api/v1/keys", requireSubject(s.handleCreateKey))
	mux.Handle("GET /api/v1/keys", requireSubject(s.handleListKeys))
	mux.Handle("DELETE /api/v1/keys/{id}", requireSubject(s.handleRevokeKey))

	// Orgs (any authenticated user can list their orgs / create an org /
	// view a specific org they belong to).
	mux.Handle("GET /api/v1/orgs", requireSubject(s.handleListOrgs))
	mux.Handle("POST /api/v1/orgs", requireSubject(s.handleCreateOrg))
	mux.Handle("GET /api/v1/orgs/{id}", requireSubject(s.handleGetOrg))
	mux.Handle("PUT /api/v1/orgs/{id}", requireSubject(s.handleUpdateOrg))

	// Members (requires org-admin or org-owner; enforced in handler).
	mux.Handle("GET /api/v1/orgs/{id}/members", requireSubject(s.handleListMembers))
	mux.Handle("POST /api/v1/orgs/{id}/members", requireSubject(s.handleAddMember))
	// The literal "me" is more specific than {email}, so it wins the match and
	// lets any member leave without holding org-admin.
	mux.Handle("DELETE /api/v1/orgs/{id}/members/me", requireSubject(s.handleLeaveOrg))
	mux.Handle("PATCH /api/v1/orgs/{id}/members/{email}", requireSubject(s.handlePatchMember))
	mux.Handle("DELETE /api/v1/orgs/{id}/members/{email}", requireSubject(s.handleRemoveMember))

	// Invitations. Creating one returns the token (email delivery is out of
	// scope); PATCH by token is how the invitee accepts or declines.
	mux.Handle("POST /api/v1/orgs/{id}/invitations", requireSubject(s.handleCreateInvitation))
	mux.Handle("GET /api/v1/orgs/{id}/invitations", requireSubject(s.handleListInvitations))
	mux.Handle("PATCH /api/v1/invitations/{token}", requireSubject(s.handlePatchInvitation))
	mux.Handle("POST /api/v1/invitations/accept", requireSubject(s.handleAcceptInvitation))

	// Hosted repos (R3 §5.1). Org-scoped rather than admin-only: these are a
	// tenant's own repositories, so any authenticated principal may ask, and
	// per-repo authorization is decided inside the handlers by acl.CanAccess
	// plus the org role ladder (see authorizeRepo). The {org} segment accepts an
	// org slug or id — the slug is what appears in a pull reference.
	//
	// Note the path parameter is {org} here, whereas the org/member routes above
	// use {id}: Go 1.22 routing requires a consistent name per path position
	// only within a single pattern, and these are distinct patterns.
	mux.Handle("GET /api/v1/orgs/{org}/repos", requireSubject(s.handleListRepos))
	mux.Handle("GET /api/v1/orgs/{org}/repos/{repo}", requireSubject(s.handleGetRepo))
	mux.Handle("PATCH /api/v1/orgs/{org}/repos/{repo}", requireSubject(s.handlePatchRepo))
	mux.Handle("DELETE /api/v1/orgs/{org}/repos/{repo}", requireSubject(s.handleDeleteRepo))
	mux.Handle("GET /api/v1/orgs/{org}/repos/{repo}/tags", requireSubject(s.handleListRepoTags))
	mux.Handle("DELETE /api/v1/orgs/{org}/repos/{repo}/tags/{tag}", requireSubject(s.handleDeleteRepoTag))
	mux.Handle("GET /api/v1/orgs/{org}/repos/{repo}/grants", requireSubject(s.handleListRepoGrants))
	mux.Handle("PUT /api/v1/orgs/{org}/repos/{repo}/grants", requireSubject(s.handleUpsertRepoGrant))
	mux.Handle("DELETE /api/v1/orgs/{org}/repos/{repo}/grants/{stype}/{sid}", requireSubject(s.handleDeleteRepoGrant))
}

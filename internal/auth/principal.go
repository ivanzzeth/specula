// principal.go extends the auth middleware with multi-tenant principal
// resolution: an acl.Subject (who is acting) and an active org ID (in which
// org) are derived from each request and injected into the context.
//
// Resolution order (first match wins):
//  1. Bearer token with "spck_" prefix → API key → apikey.Store.LookupSubject.
//  2. Bearer / cookie JWT → verify + revocation-check → derive org from the
//     X-Org-Id header ONLY. There is deliberately no DefaultOrgID fallback:
//     inventing an active org for a caller who named none produced a phantom
//     membership (/me claiming org_default with a null role for users who
//     belonged to nothing), after which every org-scoped call 403'd. Absent a
//     header the truthful answer is "no active org" and the client selects one.
//  3. No credentials → anonymous acl.Subject{} (public-read only; handlers
//     that need authentication check SubjectFromContext themselves).
//
// Existing session middleware (Middleware) and AdminRequired are unchanged; they
// remain the guard for the legacy /api/v1/auth/* and /api/v1/admin/* routes.
// PrincipalMiddleware is the entry-point for the new multi-tenant routes.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/org"
)

// ---- context keys (distinct types prevent collisions with contextKey{}) --------

// subjectCtxKey is the context key for the resolved acl.Subject.
type subjectCtxKey struct{}

// orgCtxKey is the context key for the active org ID string.
type orgCtxKey struct{}

// activeOrgCtxKey is the context key for the resolved *org.Org, carrying the
// caller's per-request Role and SystemAccess flag.
type activeOrgCtxKey struct{}

// ---- context accessors ---------------------------------------------------------

// SubjectFromContext retrieves the acl.Subject injected by PrincipalMiddleware.
// Returns (acl.Subject{}, false) when the middleware has not run or the request
// is anonymous.
func SubjectFromContext(ctx context.Context) (acl.Subject, bool) {
	s, ok := ctx.Value(subjectCtxKey{}).(acl.Subject)
	return s, ok
}

// OrgFromContext retrieves the active org ID injected by PrincipalMiddleware.
// Returns ("", false) when no org has been resolved.
func OrgFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(orgCtxKey{}).(string)
	return id, ok && id != ""
}

// ActiveOrgFromContext retrieves the resolved active organization, with Role and
// SystemAccess populated for this request. Returns (nil, false) when the caller
// named no org (no X-Org-Id) or is not entitled to one — mirroring ai-sandbox's
// resolveHumanOrg, whose nil result means "no active org", never "org_default".
func ActiveOrgFromContext(ctx context.Context) (*org.Org, bool) {
	o, ok := ctx.Value(activeOrgCtxKey{}).(*org.Org)
	return o, ok && o != nil
}

// ---- org resolver interface (narrow slice of org.Store) ------------------------

// OrgResolver is the subset of org.Store required by PrincipalMiddleware to
// resolve the active org and membership role for session-authenticated callers.
// Any org.Store implementation satisfies this interface.
type OrgResolver interface {
	GetOrg(ctx context.Context, id string) (*org.Org, error)
	GetOrgMember(ctx context.Context, orgID, email string) (*org.Member, error)
	ListOrgsForEmail(ctx context.Context, email string) ([]*org.Org, error)
}

// ---- principal middleware -------------------------------------------------------

// PrincipalMiddleware resolves the caller's acl.Subject and active org for each
// request, injecting them via SubjectFromContext / OrgFromContext. It also sets
// the auth.User in context (same private key as Middleware) when a valid JWT is
// presented, so handlers can call UserFromContext without chaining both middlewares.
//
// Parameters:
//   - keys     — API-key store; nil-safe (apikey path skipped when nil).
//   - orgs     — org resolver; nil-safe (X-Org-Id membership check skipped).
//   - verifier — JWT signer/verifier shared with the session middleware.
//   - users    — user store for revocation check.
func PrincipalMiddleware(keys apikey.Store, orgs OrgResolver, verifier TokenVerifier, users UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)

			var subject acl.Subject
			var activeOrgID string
			var activeOrg *org.Org

			switch {
			case token != "" && keys != nil && strings.HasPrefix(token, apikey.KeyPrefix):
				// ── API-key path (spck_ prefix) ───────────────────────────────
				orgID, subj, ok := keys.LookupSubject(token)
				if !ok {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				// API keys are org-admin within their pinned org; X-Org-Id is
				// ignored for keys (the key pins its org).
				subject = acl.Subject{UserID: subj, OrgID: orgID, Admin: false}
				activeOrgID = orgID
				// fail-CLOSED (mirrors resolveKeyOrg): if the pinned org row is
				// gone, do NOT synthesise an active+admin phantom org — a key
				// would otherwise hold org-admin over an org that no longer
				// exists. Leave activeOrg nil; the frozen check below still runs.
				if orgs != nil {
					if o, err := orgs.GetOrg(r.Context(), orgID); err == nil {
						// A frozen org seals machine access too — a key is the
						// one credential most likely to keep hammering a
						// suspended tenant.
						if o.Frozen() {
							http.Error(w, "organization is frozen", http.StatusForbidden)
							return
						}
						o.Role = org.RoleAdmin
						activeOrg = o
					}
				}

			case token != "":
				// ── JWT / session path ────────────────────────────────────────
				embedded, err := verifier.Verify(token)
				if err != nil {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				current, err := users.GetUserByID(r.Context(), embedded.ID)
				if err != nil || current.TokenGen != embedded.TokenGen {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				// Inject auth.User into context so legacy UserFromContext calls
				// in downstream handlers continue to work.
				r = r.WithContext(context.WithValue(r.Context(), contextKey{}, *current))

				// Resolve the active org from X-Org-Id ONLY (mirrors
				// resolveHumanOrg). No header → no active org: the caller named
				// none, and inventing org_default here is what made /me report a
				// membership that did not exist.
				orgID := strings.TrimSpace(r.Header.Get("X-Org-Id"))
				if orgID != "" && orgs != nil {
					o, err := orgs.GetOrg(r.Context(), orgID)
					switch {
					case errors.Is(err, org.ErrNotFound):
						http.Error(w, "organization not found", http.StatusNotFound)
						return
					case err != nil:
						// A lookup failure is not "you picked a bad org" — say so
						// truthfully rather than sending a legitimate member off
						// to re-pick a workspace that was never the problem.
						http.Error(w, "cannot look up your organization right now; try again shortly",
							http.StatusServiceUnavailable)
						return
					}

					sysRole := org.NormalizeSystemRole(current.SystemRole)
					m, memErr := orgs.GetOrgMember(r.Context(), orgID, current.Email)
					switch {
					case memErr == nil:
						o.Role = org.NormalizeRole(m.Role)
					case !errors.Is(memErr, org.ErrNotFound):
						http.Error(w, "cannot verify your membership right now; try again shortly",
							http.StatusServiceUnavailable)
						return
					case sysRole != "":
						// System-role holders get implicit cross-org read-only
						// access: viewer, flagged SystemAccess so member/key
						// management does not treat them as a real member.
						o.Role = org.RoleViewer
						o.SystemAccess = true
					default:
						// Named an org they have no claim on. Fail closed.
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}

					// Frozen orgs seal all access, reads included. System admins
					// keep an operational break-glass channel.
					if o.Frozen() && sysRole != org.RoleAdmin {
						http.Error(w, "organization is frozen", http.StatusForbidden)
						return
					}

					activeOrg = o
					activeOrgID = o.ID
				}

				subject = acl.Subject{
					UserID: org.UserSubjectID(current.ID),
					OrgID:  activeOrgID,
					Admin:  org.NormalizeSystemRole(current.SystemRole) == org.RoleAdmin,
				}

			default:
				// ── Anonymous ─────────────────────────────────────────────────
				subject = acl.Subject{}
				activeOrgID = ""
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, subjectCtxKey{}, subject)
			ctx = context.WithValue(ctx, orgCtxKey{}, activeOrgID)
			ctx = context.WithValue(ctx, activeOrgCtxKey{}, activeOrg)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

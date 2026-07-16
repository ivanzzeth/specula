// principal.go extends the auth middleware with multi-tenant principal
// resolution: an acl.Subject (who is acting) and an active org ID (in which
// org) are derived from each request and injected into the context.
//
// Resolution order (first match wins):
//  1. Bearer token with "spck_" prefix → API key → apikey.Store.LookupSubject.
//  2. Bearer / cookie JWT → verify + revocation-check → derive org from the
//     X-Org-Id header (or fall back to DefaultOrgID).
//  3. No credentials → anonymous acl.Subject{} (public-read only; handlers
//     that need authentication check SubjectFromContext themselves).
//
// Existing session middleware (Middleware) and AdminRequired are unchanged; they
// remain the guard for the legacy /api/v1/auth/* and /api/v1/admin/* routes.
// PrincipalMiddleware is the entry-point for the new multi-tenant routes.
package auth

import (
	"context"
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

// ---- org resolver interface (narrow slice of org.Store) ------------------------

// OrgResolver is the subset of org.Store required by PrincipalMiddleware to
// resolve the active org and membership role for session-authenticated callers.
// Any org.Store implementation satisfies this interface.
type OrgResolver interface {
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

				// Resolve active org: X-Org-Id header takes precedence; fall back
				// to DefaultOrgID when absent.
				orgID := r.Header.Get("X-Org-Id")
				explicitOrg := orgID != ""
				if orgID == "" {
					orgID = org.DefaultOrgID
				}

				if orgs != nil {
					_, memErr := orgs.GetOrgMember(r.Context(), orgID, current.Email)
					if memErr != nil {
						// Not a member of the requested org.
						if current.SystemRole == "admin" {
							// System admins get implicit cross-org read-only access.
						} else if explicitOrg {
							// Explicit org requested but caller is not a member.
							http.Error(w, "forbidden", http.StatusForbidden)
							return
						}
						// Non-explicit default org miss: continue (handler decides).
					}
				}

				subject = acl.Subject{
					UserID: org.UserSubjectID(current.ID),
					OrgID:  orgID,
					Admin:  current.SystemRole == "admin",
				}
				activeOrgID = orgID

			default:
				// ── Anonymous ─────────────────────────────────────────────────
				subject = acl.Subject{}
				activeOrgID = ""
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, subjectCtxKey{}, subject)
			ctx = context.WithValue(ctx, orgCtxKey{}, activeOrgID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

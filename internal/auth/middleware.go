package auth

import (
	"context"
	"net/http"
	"strings"
)

// contextKey is an unexported type to prevent key collisions in context values.
type contextKey struct{}

// UserFromContext retrieves the authenticated User set by Middleware.
// Returns (User{}, false) when the request has not been authenticated.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}

// Middleware returns an http.Handler middleware that authenticates each
// request via the session cookie or an Authorization: Bearer <token> header.
//
// Authentication pipeline:
//  1. Extract the raw JWT (cookie preferred, then Bearer header).
//  2. Verify the JWT signature, expiry, issuer, and audience.
//  3. Revocation check: compare the token's embedded TokenGen snapshot with
//     the live value from the store. A mismatch means Logout was called and
//     all sessions have been invalidated.
//  4. On success, set the live User (from the store) in the request context
//     so downstream handlers can call UserFromContext.
//
// Any failure at any step responds with 401 Unauthorized (no detail leaked).
func Middleware(verifier TokenVerifier, store UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			embedded, err := verifier.Verify(token)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			// Revocation check: fetch the live user and compare token_gen
			// snapshots. If BumpTokenGen was called (Logout), the embedded
			// gen is now stale and the token is rejected.
			current, err := store.GetUserByID(r.Context(), embedded.ID)
			if err != nil || current.TokenGen != embedded.TokenGen {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), contextKey{}, *current)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AdminRequired is a middleware that must be chained after Middleware. It
// ensures the authenticated user holds system_role="admin", returning 403
// Forbidden otherwise.
func AdminRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok || u.SystemRole != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSessionCookie writes the session JWT as an httpOnly, SameSite=Lax cookie.
// The Secure attribute is set when secure is true (should be true for HTTPS
// deployments to prevent the cookie from being transmitted over plain HTTP).
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     TokenCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(DefaultTokenTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// ClearSessionCookie expires the session cookie on the client, logging the
// browser out. For server-side revocation of all sessions use Service.Logout
// which bumps the token_gen generation counter.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     TokenCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// extractToken pulls the raw JWT from the request: the session cookie is
// preferred (browser clients), falling back to an Authorization: Bearer <token>
// header (API/CLI clients).
func extractToken(r *http.Request) string {
	if c, err := r.Cookie(TokenCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

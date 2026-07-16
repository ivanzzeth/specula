package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---- JWT test helper ----------------------------------------------------------

// signRaw assembles a compact JWT with the given alg header field and payload,
// signing with a real HMAC-SHA256 regardless of the alg string. This lets us
// construct tokens whose alg claim differs from the actual signing algorithm
// (used for alg-confusion rejection tests: the sig is valid but the alg tag is
// wrong, so a correct implementation must reject on alg before checking sig).
func signRaw(t *testing.T, secret []byte, alg string, payload map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	pl, _ := json.Marshal(payload)
	unsigned := b64.EncodeToString(hdr) + "." + b64.EncodeToString(pl)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + b64.EncodeToString(mac.Sum(nil))
}

// validPayload returns a minimal claims map for signRaw.
func validPayload(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"uid":   float64(1),
		"email": "test@example.com",
		"role":  "user",
		"iss":   tokenIssuer,
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

// ---- fakeStore ----------------------------------------------------------------

// fakeStore is an in-process UserStore used in tests. It is safe for concurrent
// use. It mirrors the exact semantics expected by Service: GetUserByEmail
// returns ErrUserNotFound on miss; CreateUser returns ErrEmailTaken on dup;
// BumpTokenGen atomically increments the gen of both indexes.
type fakeStore struct {
	mu    sync.Mutex
	users map[string]*User // keyed by normalised email
	byID  map[int64]*User
	seq   int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users: make(map[string]*User),
		byID:  make(map[int64]*User),
	}
}

func (f *fakeStore) CountUsers(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.users)), nil
}

func (f *fakeStore) GetUserByEmail(_ context.Context, email string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[email]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeStore) CreateUser(_ context.Context, u User) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.users[u.Email]; dup {
		return nil, ErrEmailTaken
	}
	f.seq++
	u.ID = f.seq
	// Both maps point to the same heap-allocated User so BumpTokenGen only
	// needs to update through one pointer.
	stored := u
	f.users[u.Email] = &stored
	f.byID[u.ID] = &stored
	return &stored, nil
}

func (f *fakeStore) GetUserByID(_ context.Context, id int64) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeStore) BumpTokenGen(_ context.Context, id int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return 0, ErrUserNotFound
	}
	u.TokenGen++ // both maps share the same *User so the email index is updated too
	return u.TokenGen, nil
}

func (f *fakeStore) ListUsers(_ context.Context, limit, offset int) ([]User, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.byID))
	for id := range f.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	total := int64(len(ids))
	if offset > len(ids) {
		offset = len(ids)
	}
	ids = ids[offset:]
	if limit > 0 && limit < len(ids) {
		ids = ids[:limit]
	}
	out := make([]User, 0, len(ids))
	for _, id := range ids {
		out = append(out, *f.byID[id])
	}
	return out, total, nil
}

func (f *fakeStore) UpdateUserRole(_ context.Context, id int64, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	u.SystemRole = role
	return nil
}

func (f *fakeStore) UpdateUserFields(_ context.Context, id int64, name, passwordHash *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	if name != nil {
		u.Name = *name
		if byEmail, ok2 := f.users[u.Email]; ok2 {
			byEmail.Name = *name
		}
	}
	if passwordHash != nil {
		u.PasswordHash = *passwordHash
		if byEmail, ok2 := f.users[u.Email]; ok2 {
			byEmail.PasswordHash = *passwordHash
		}
	}
	return nil
}

func (f *fakeStore) DeleteUser(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	delete(f.byID, id)
	delete(f.users, u.Email)
	return nil
}

// ---- service factory ----------------------------------------------------------

func newTestService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("test-hs256-secret")), false)
	return svc, store
}

// ---- HS256 verifier: sign/verify round-trip -----------------------------------

func TestHS256Verifier_SignVerify_RoundTrip(t *testing.T) {
	v := NewHS256Verifier([]byte("round-trip-secret"))
	original := User{ID: 42, Email: "alice@example.com", SystemRole: "admin", TokenGen: 7}

	tok, err := v.Sign(original)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ID != original.ID {
		t.Errorf("ID: got %d, want %d", got.ID, original.ID)
	}
	if got.Email != original.Email {
		t.Errorf("Email: got %q, want %q", got.Email, original.Email)
	}
	if got.SystemRole != original.SystemRole {
		t.Errorf("SystemRole: got %q, want %q", got.SystemRole, original.SystemRole)
	}
	if got.TokenGen != original.TokenGen {
		t.Errorf("TokenGen: got %d, want %d", got.TokenGen, original.TokenGen)
	}
}

// ---- HS256 verifier: alg-confusion / rejection tests -------------------------

func TestHS256Verifier_AlgNoneRejected(t *testing.T) {
	secret := []byte("alg-test-secret")
	v := NewHS256Verifier(secret)

	// alg=none with HMAC signature still present (hardest case: valid sig, bad alg).
	tok := signRaw(t, secret, "none", validPayload(t))
	_, err := v.Verify(tok)
	if !errors.Is(err, ErrAlg) {
		t.Fatalf("alg=none: got %v, want ErrAlg", err)
	}
}

func TestHS256Verifier_AsymmetricAlgsRejected(t *testing.T) {
	secret := []byte("alg-test-secret")
	v := NewHS256Verifier(secret)
	pl := validPayload(t)

	cases := []string{"RS256", "RS512", "ES256", "ES512", "PS256", "EdDSA"}
	for _, alg := range cases {
		t.Run(alg, func(t *testing.T) {
			tok := signRaw(t, secret, alg, pl)
			_, err := v.Verify(tok)
			if !errors.Is(err, ErrAlg) {
				t.Fatalf("alg=%s: got %v, want ErrAlg", alg, err)
			}
		})
	}
}

func TestHS256Verifier_LowercaseAlgRejected(t *testing.T) {
	// "hs256" vs "HS256" — algorithm names are case-sensitive per RFC 7518.
	secret := []byte("alg-test-secret")
	v := NewHS256Verifier(secret)
	tok := signRaw(t, secret, "hs256", validPayload(t))
	if _, err := v.Verify(tok); !errors.Is(err, ErrAlg) {
		t.Fatalf("alg=hs256 (lowercase): got %v, want ErrAlg", err)
	}
}

func TestHS256Verifier_WrongSecretRejected(t *testing.T) {
	v1 := NewHS256Verifier([]byte("secret-A"))
	v2 := NewHS256Verifier([]byte("secret-B"))
	tok, err := v1.Sign(User{ID: 1, Email: "x@x.com", SystemRole: "user"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := v2.Verify(tok); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong secret: got %v, want ErrSignature", err)
	}
}

func TestHS256Verifier_ExpiredRejected(t *testing.T) {
	secret := []byte("test-secret")
	v := NewHS256Verifier(secret)
	// exp well before now (beyond the clockSkew tolerance).
	tok := signRaw(t, secret, "HS256", map[string]any{
		"uid": float64(1), "email": "x@x.com", "role": "user",
		"iss": tokenIssuer,
		"exp": time.Now().Add(-2 * time.Hour).Unix(),
	})
	if _, err := v.Verify(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired token: got %v, want ErrExpired", err)
	}
}

func TestHS256Verifier_WrongIssuerRejected(t *testing.T) {
	secret := []byte("test-secret")
	v := NewHS256Verifier(secret)
	tok := signRaw(t, secret, "HS256", map[string]any{
		"uid": float64(1), "email": "x@x.com", "role": "user",
		"iss": "evil-issuer",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(tok); !errors.Is(err, ErrIssuer) {
		t.Fatalf("wrong issuer: got %v, want ErrIssuer", err)
	}
}

func TestHS256Verifier_WrongAudienceRejected(t *testing.T) {
	secret := []byte("test-secret")
	v := NewHS256Verifier(secret)
	tok := signRaw(t, secret, "HS256", map[string]any{
		"uid": float64(1), "email": "x@x.com", "role": "user",
		"iss": tokenIssuer, "aud": "wrong-service",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(tok); !errors.Is(err, ErrAudience) {
		t.Fatalf("wrong aud: got %v, want ErrAudience", err)
	}
}

func TestHS256Verifier_MissingAudienceAccepted(t *testing.T) {
	// Tokens issued without an aud claim must remain valid for backward
	// compatibility. A missing aud is benign because forgery still requires
	// the HMAC key.
	secret := []byte("test-secret")
	v := NewHS256Verifier(secret)
	tok := signRaw(t, secret, "HS256", map[string]any{
		"uid": float64(1), "email": "x@x.com", "role": "user",
		"iss": tokenIssuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("token without aud must verify, got %v", err)
	}
}

func TestHS256Verifier_Malformed(t *testing.T) {
	v := NewHS256Verifier([]byte("s"))
	// All of these must return ErrMalformed regardless of content.
	cases := []string{
		"",              // empty
		"not-a-jwt",     // no dots
		"a.b",           // only two segments
		"a.b.c.d",       // four segments
		"..",            // empty segments
		"a..c",          // empty payload
		"!invalid-b64.", // invalid base64 in header
	}
	for _, tc := range cases {
		if _, err := v.Verify(tc); !errors.Is(err, ErrMalformed) {
			t.Errorf("Verify(%q): got %v, want ErrMalformed", tc, err)
		}
	}
}

// ---- bcrypt hasher ------------------------------------------------------------

func TestBcryptHasher_HashAndCompare(t *testing.T) {
	h := NewBcryptHasher()
	const pw = "correct horse battery staple"
	hash, err := h.Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := h.Compare(hash, pw); err != nil {
		t.Fatal("Compare should accept the original password")
	}
	if err := h.Compare(hash, "wrong-password"); err == nil {
		t.Fatal("Compare should reject a different password")
	}
}

func TestBcryptHasher_EmptyHashRejected(t *testing.T) {
	h := NewBcryptHasher()
	if err := h.Compare("", "anypassword"); err == nil {
		t.Fatal("Compare with empty hash must return an error, not nil")
	}
}

func TestBcryptHasher_TooLongPassword(t *testing.T) {
	// bcrypt silently truncates at 72 bytes; GenerateFromPassword returns an
	// error for inputs > 72 bytes to prevent false-pass on near-identical long
	// passwords (Go >=1.22 behaviour).
	h := NewBcryptHasher()
	longPW := make([]byte, 73)
	for i := range longPW {
		longPW[i] = 'a'
	}
	if _, err := h.Hash(string(longPW)); err == nil {
		t.Fatal("Hash accepted a >72-byte password, want error")
	}
}

// ---- password.go helpers ------------------------------------------------------

func TestCheckPasswordDummy_Runs(t *testing.T) {
	// Verify it does not panic and returns; actual timing equalization is a
	// runtime security property, not a unit-testable invariant.
	CheckPasswordDummy()
}

// ---- service: registration ----------------------------------------------------

func TestService_FirstUserAdmin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// The very first registered account must receive system_role="admin".
	u1, err := svc.Register(ctx, "admin@example.com", "password123!", "Admin User")
	if err != nil {
		t.Fatalf("Register first user: %v", err)
	}
	if u1.SystemRole != "admin" {
		t.Fatalf("first user SystemRole=%q, want admin", u1.SystemRole)
	}
	if u1.Name != "Admin User" {
		t.Fatalf("first user Name=%q, want Admin User", u1.Name)
	}

	// All subsequent accounts must be plain users.
	u2, err := svc.Register(ctx, "user@example.com", "password123!", "")
	if err != nil {
		t.Fatalf("Register second user: %v", err)
	}
	if u2.SystemRole != "user" {
		t.Fatalf("second user SystemRole=%q, want user", u2.SystemRole)
	}
}

func TestService_Register_DuplicateEmail(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "dup@example.com", "password123!", ""); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := svc.Register(ctx, "dup@example.com", "password123!", "")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: got %v, want ErrEmailTaken", err)
	}
}

func TestService_Register_PasswordTooShort(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Register(context.Background(), "x@example.com", "short", "")
	if !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password: got %v, want ErrPasswordTooShort", err)
	}
}

func TestService_Register_ExactMinLength(t *testing.T) {
	svc, _ := newTestService(t)
	// Exactly MinPasswordLen bytes must be accepted.
	pw := make([]byte, MinPasswordLen)
	for i := range pw {
		pw[i] = 'x'
	}
	if _, err := svc.Register(context.Background(), "exact@example.com", string(pw), ""); err != nil {
		t.Fatalf("password of exactly MinPasswordLen: got %v, want nil", err)
	}
}

func TestService_Register_EmailRequired(t *testing.T) {
	svc, _ := newTestService(t)
	cases := []string{"", "   ", "\t"}
	for _, email := range cases {
		if _, err := svc.Register(context.Background(), email, "password123!", ""); !errors.Is(err, ErrEmailRequired) {
			t.Errorf("blank email %q: got %v, want ErrEmailRequired", email, err)
		}
	}
}

func TestService_Register_EmailNormalised(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	// Emails are lower-cased and trimmed before storage/duplicate checks.
	u, err := svc.Register(ctx, "  Alice@Example.COM  ", "password123!", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("email not normalised: got %q, want alice@example.com", u.Email)
	}
	// Re-registering the same email in different case must be detected as dup.
	if _, err := svc.Register(ctx, "ALICE@EXAMPLE.COM", "password123!", ""); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("case-insensitive dup: got %v, want ErrEmailTaken", err)
	}
}

// ---- service: login -----------------------------------------------------------

func TestService_Login_Success(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "carol@example.com", "carolspassword1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, u, err := svc.Login(ctx, "carol@example.com", "carolspassword1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok == "" {
		t.Fatal("Login returned empty token")
	}
	if u.Email != "carol@example.com" {
		t.Fatalf("Login returned wrong user email: %q", u.Email)
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "dave@example.com", "correctpass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := svc.Login(ctx, "dave@example.com", "wrongpass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: got %v, want ErrInvalidCredentials", err)
	}
}

// TestService_Login_NotFound_TimingSafe verifies that Login returns the
// identical sentinel error regardless of whether the failure was "no such user"
// or "wrong password". The code path also calls CheckPasswordDummy() in the
// not-found branch to consume equivalent CPU time (equalises response latency;
// see password.go). The latency property itself is a runtime guarantee that
// cannot be asserted in a unit test — we assert the error identity here.
func TestService_Login_NotFound_TimingSafe(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// Create a real user so the "user not found" path can be compared against
	// the "wrong password" path (both must return ErrInvalidCredentials).
	if _, err := svc.Register(ctx, "real@example.com", "realpassword1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, _, errNotFound := svc.Login(ctx, "ghost@example.com", "anypassword")
	_, _, errWrongPwd := svc.Login(ctx, "real@example.com", "wrongpassword")

	if !errors.Is(errNotFound, ErrInvalidCredentials) {
		t.Fatalf("not-found path: got %v, want ErrInvalidCredentials", errNotFound)
	}
	if !errors.Is(errWrongPwd, ErrInvalidCredentials) {
		t.Fatalf("wrong-password path: got %v, want ErrInvalidCredentials", errWrongPwd)
	}
	// Same sentinel value — callers cannot distinguish the two failure modes.
	if errNotFound != errWrongPwd {
		t.Fatalf("errors differ: notFound=%v wrongPwd=%v — both must be ErrInvalidCredentials",
			errNotFound, errWrongPwd)
	}
}

// ---- service: token_gen revocation -------------------------------------------

// TestService_TokenGenRevocation confirms that after Logout, the embedded
// token_gen in an old JWT no longer matches the live store value, so the
// Middleware will reject it with 401.
func TestService_TokenGenRevocation(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "eve@example.com", "evepassword1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, u, err := svc.Login(ctx, "eve@example.com", "evepassword1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Before logout: embedded TokenGen must match the store.
	embedded, err := svc.verifier.Verify(tok)
	if err != nil {
		t.Fatalf("Verify before logout: %v", err)
	}
	before, _ := store.GetUserByID(ctx, u.ID)
	if embedded.TokenGen != before.TokenGen {
		t.Fatalf("pre-logout gen mismatch: token=%d store=%d", embedded.TokenGen, before.TokenGen)
	}

	// Logout bumps the gen.
	if err := svc.Logout(ctx, u.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// After logout: embedded gen must now differ from the store gen.
	after, _ := store.GetUserByID(ctx, u.ID)
	if embedded.TokenGen == after.TokenGen {
		t.Fatal("token_gen must have changed after Logout; old token must be rejected by Middleware")
	}
	if after.TokenGen != before.TokenGen+1 {
		t.Fatalf("expected gen to increment by 1: before=%d after=%d", before.TokenGen, after.TokenGen)
	}
}

// ---- middleware ---------------------------------------------------------------

func TestMiddleware_NoToken_Returns401(t *testing.T) {
	store := newFakeStore()
	mw := Middleware(NewHS256Verifier([]byte("s")), store)
	rec := httptest.NewRecorder()
	mw(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
}

func TestMiddleware_InvalidToken_Returns401(t *testing.T) {
	store := newFakeStore()
	mw := Middleware(NewHS256Verifier([]byte("s")), store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookieName, Value: "not.a.jwt"})
	mw(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: want 401, got %d", rec.Code)
	}
}

func TestMiddleware_ValidCookie_SetsContext(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "frank@example.com", "frankpass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "frank@example.com", "frankpass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var ctxUser User
	handler := Middleware(svc.verifier, store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxUser, _ = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookieName, Value: tok})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid cookie: want 200, got %d", rec.Code)
	}
	if ctxUser.Email != "frank@example.com" {
		t.Fatalf("context user email=%q, want frank@example.com", ctxUser.Email)
	}
}

func TestMiddleware_ValidBearerToken(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "grace@example.com", "gracepass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "grace@example.com", "gracepass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	handler := Middleware(svc.verifier, store)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer auth: want 200, got %d", rec.Code)
	}
}

// TestMiddleware_TokenGenRevocation is the end-to-end HTTP test for the
// logout-all revocation mechanism: after Logout bumps token_gen, the old
// session cookie must result in 401.
func TestMiddleware_TokenGenRevocation(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "henry@example.com", "henrypass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, u, err := svc.Login(ctx, "henry@example.com", "henrypass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := svc.Logout(ctx, u.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	handler := Middleware(svc.verifier, store)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookieName, Value: tok})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: want 401, got %d", rec.Code)
	}
}

// ---- AdminRequired middleware --------------------------------------------------

func TestAdminRequired_Admin_Passes(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	// First user = admin.
	if _, err := svc.Register(ctx, "admin@example.com", "adminpass1", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, _, err := svc.Login(ctx, "admin@example.com", "adminpass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	chain := Middleware(svc.verifier, store)(AdminRequired(okHandler()))
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookieName, Value: tok})
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin user: want 200, got %d", rec.Code)
	}
}

func TestAdminRequired_NonAdmin_Returns403(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	// Register admin (to ensure the next user is "user").
	if _, err := svc.Register(ctx, "admin@example.com", "adminpass1", ""); err != nil {
		t.Fatalf("Register admin: %v", err)
	}
	if _, err := svc.Register(ctx, "plain@example.com", "plainpass1", ""); err != nil {
		t.Fatalf("Register plain user: %v", err)
	}
	tok, _, err := svc.Login(ctx, "plain@example.com", "plainpass1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	chain := Middleware(svc.verifier, store)(AdminRequired(okHandler()))
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookieName, Value: tok})
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin user: want 403, got %d", rec.Code)
	}
}

// ---- cookie helpers -----------------------------------------------------------

func TestSetSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok123", false)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != TokenCookieName {
		t.Errorf("Name=%q, want %q", c.Name, TokenCookieName)
	}
	if c.Value != "tok123" {
		t.Errorf("Value=%q, want tok123", c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if c.Secure {
		t.Error("Secure must be false when secure=false")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite=%v, want Lax", c.SameSite)
	}
}

func TestSetSessionCookie_Secure(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok", true)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("Secure flag must be set when secure=true")
	}
}

func TestClearSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	if cookies[0].MaxAge != -1 {
		t.Errorf("MaxAge=%d, want -1 (expire immediately)", cookies[0].MaxAge)
	}
	if cookies[0].Value != "" {
		t.Errorf("Value=%q, want empty", cookies[0].Value)
	}
}

// ---- helpers ------------------------------------------------------------------

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

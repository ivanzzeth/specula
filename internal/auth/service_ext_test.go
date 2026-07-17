package auth

// service_ext_test.go adds test coverage for branches not reached by auth_test.go:
//   - Service.Secure()
//   - Service.Register with orgStore (first-user default-org bootstrap)
//   - Service.Register with orgStore (second user must NOT auto-join) [REGISTRY-DESIGN §2.2]
//   - Service.Register hash error path (>72 byte password)
//   - Service.Register GetUserByEmail unexpected error path
//   - Service.Register CountUsers error path
//   - Service.Register CreateUser error path
//   - Service.Logout error (user not found)
//   - mapHS256ParseError: ErrTokenMalformed branch via library (invalid payload JSON)

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/org"
)

// ── Service.Secure ────────────────────────────────────────────────────────────

func TestService_Secure(t *testing.T) {
	secureTrue := NewService(newFakeStore(), NewBcryptHasher(), NewHS256Verifier([]byte("s")), true)
	if !secureTrue.Secure() {
		t.Error("Secure() must return true when constructed with secure=true")
	}

	secureFalse := NewService(newFakeStore(), NewBcryptHasher(), NewHS256Verifier([]byte("s")), false)
	if secureFalse.Secure() {
		t.Error("Secure() must return false when constructed with secure=false")
	}
}

// ── Service.Register with orgStore ───────────────────────────────────────────

// TestService_FirstUser_CreatesDefaultOrg verifies that when the very first user
// registers and an orgStore is wired in, the service creates the default org and
// adds the user as owner. This is the multi-tenancy bootstrap described in
// REGISTRY-DESIGN §2.2.
func TestService_FirstUser_CreatesDefaultOrg(t *testing.T) {
	store := newFakeStore()
	orgStore := org.NewMemStore()
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("s")), false, orgStore)
	ctx := context.Background()

	u, err := svc.Register(ctx, "admin@example.com", "password123!", "Admin")
	if err != nil {
		t.Fatalf("Register first user: %v", err)
	}

	// The default org must exist.
	o, err := orgStore.GetOrg(ctx, org.DefaultOrgID)
	if err != nil {
		t.Fatalf("GetOrg: %v", err)
	}
	if o.Slug != org.DefaultOrgSlug {
		t.Errorf("default org slug: got %q, want %q", o.Slug, org.DefaultOrgSlug)
	}

	// The first user must be the org owner.
	m, err := orgStore.GetOrgMember(ctx, org.DefaultOrgID, u.Email)
	if err != nil {
		t.Fatalf("GetOrgMember: %v", err)
	}
	if m.Role != org.RoleOwner {
		t.Errorf("first user org role: got %q, want owner", m.Role)
	}
}

// TestService_SecondUser_NotAutoJoinOrg pins the requirement from REGISTRY-DESIGN
// §2.2: only the FIRST registered user auto-joins the default org. Every
// subsequent user is invitation-only. Auto-joining later registrants would hand
// anyone who can reach the sign-up page push access to the default org — that
// is the security invariant being tested here.
func TestService_SecondUser_NotAutoJoinOrg(t *testing.T) {
	store := newFakeStore()
	orgStore := org.NewMemStore()
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("s")), false, orgStore)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "admin@example.com", "password123!", ""); err != nil {
		t.Fatalf("Register first user: %v", err)
	}
	u2, err := svc.Register(ctx, "user2@example.com", "password123!", "")
	if err != nil {
		t.Fatalf("Register second user: %v", err)
	}

	// Second user must NOT appear as a member of the default org.
	_, err = orgStore.GetOrgMember(ctx, org.DefaultOrgID, u2.Email)
	if !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("second user must not auto-join default org; got err=%v (want ErrNotFound)", err)
	}
}

// ── Service.Register hash-error path ─────────────────────────────────────────

// TestService_Register_HashError covers the "auth: hash password: …" branch in
// Register, triggered when bcrypt rejects a >72-byte password. The user account
// must not be created in this case.
func TestService_Register_HashError(t *testing.T) {
	svc, store := newTestService(t)
	// A 73-byte password passes MinPasswordLen but fails bcrypt (Go >=1.22).
	longPW := strings.Repeat("a", 73)
	_, err := svc.Register(context.Background(), "hashfail@example.com", longPW, "")
	if err == nil {
		t.Fatal("Register with >72 byte password must return an error")
	}
	// Confirm no user row was created.
	if _, gerr := store.GetUserByEmail(context.Background(), "hashfail@example.com"); !errors.Is(gerr, ErrUserNotFound) {
		t.Errorf("no user row must exist after a hash error; got gerr=%v", gerr)
	}
}

// ── Service.Register store-error paths ───────────────────────────────────────

// errableStore wraps fakeStore and injects specific errors on demand.
type errableStore struct {
	*fakeStore
	emailLookupErr error // GetUserByEmail unexpected error
	countErr       error // CountUsers error
	createErr      error // CreateUser error
}

func (e *errableStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	if e.emailLookupErr != nil {
		return nil, e.emailLookupErr
	}
	return e.fakeStore.GetUserByEmail(ctx, email)
}

func (e *errableStore) CountUsers(ctx context.Context) (int64, error) {
	if e.countErr != nil {
		return 0, e.countErr
	}
	return e.fakeStore.CountUsers(ctx)
}

func (e *errableStore) CreateUser(ctx context.Context, u User) (*User, error) {
	if e.createErr != nil {
		return nil, e.createErr
	}
	return e.fakeStore.CreateUser(ctx, u)
}

func TestService_Register_UnexpectedEmailLookupError(t *testing.T) {
	dbErr := errors.New("db: connection reset")
	store := &errableStore{fakeStore: newFakeStore(), emailLookupErr: dbErr}
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("s")), false)

	_, err := svc.Register(context.Background(), "x@example.com", "password123!", "")
	if err == nil || !errors.Is(err, dbErr) {
		t.Fatalf("unexpected email lookup error: want wrapping %v, got %v", dbErr, err)
	}
}

func TestService_Register_CountUsersError(t *testing.T) {
	dbErr := errors.New("db: cannot count")
	store := &errableStore{fakeStore: newFakeStore(), countErr: dbErr}
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("s")), false)

	_, err := svc.Register(context.Background(), "x@example.com", "password123!", "")
	if err == nil || !errors.Is(err, dbErr) {
		t.Fatalf("CountUsers error: want wrapping %v, got %v", dbErr, err)
	}
}

func TestService_Register_CreateUserError(t *testing.T) {
	dbErr := errors.New("db: insert conflict")
	store := &errableStore{fakeStore: newFakeStore(), createErr: dbErr}
	svc := NewService(store, NewBcryptHasher(), NewHS256Verifier([]byte("s")), false)

	_, err := svc.Register(context.Background(), "x@example.com", "password123!", "")
	if err == nil || !errors.Is(err, dbErr) {
		t.Fatalf("CreateUser error: want wrapping %v, got %v", dbErr, err)
	}
}

// ── Service.Logout error path ─────────────────────────────────────────────────

func TestService_Logout_UserNotFound_ReturnsError(t *testing.T) {
	svc, _ := newTestService(t)
	// User ID 99999 does not exist.
	err := svc.Logout(context.Background(), 99999)
	if err == nil {
		t.Fatal("Logout of non-existent user must return an error")
	}
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("Logout error must wrap ErrUserNotFound; got %v", err)
	}
}

// ── mapHS256ParseError: ErrTokenMalformed branch ──────────────────────────────

// TestHS256Verifier_MalformedPayload_Returns_ErrMalformed exercises the
// jwt.ErrTokenMalformed case inside mapHS256ParseError. The token constructed
// here has a valid HS256 header and a valid HMAC signature, but the payload is
// not valid JSON. Our pre-check passes (alg=HS256); the library's JSON unmarshal
// step then fails with ErrTokenMalformed, which mapHS256ParseError maps to the
// ErrMalformed sentinel. This test pins that the mapping is correct so a future
// change to the library's error type is not silently swallowed.
func TestHS256Verifier_MalformedPayload_Returns_ErrMalformed(t *testing.T) {
	secret := []byte("map-test-secret")
	v := NewHS256Verifier(secret)

	// Construct a JWT with valid header (alg=HS256), invalid payload JSON,
	// and a valid HMAC signature.
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	badPayload := []byte("not-valid-json-claims")
	unsigned := b64.EncodeToString(hdr) + "." + b64.EncodeToString(badPayload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	tok := unsigned + "." + b64.EncodeToString(mac.Sum(nil))

	_, err := v.Verify(tok)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed payload: got %v, want ErrMalformed", err)
	}
}

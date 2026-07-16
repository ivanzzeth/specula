package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
)

// ---- fake dependencies -------------------------------------------------------

// fakeHasher is a trivial PasswordHasher that avoids bcrypt for fast tests.
type fakeHasher struct{}

func (h *fakeHasher) Hash(password string) (string, error) { return "hash:" + password, nil }
func (h *fakeHasher) Compare(hash, password string) error {
	if hash != "hash:"+password {
		return errors.New("wrong password")
	}
	return nil
}

// fakeUserStore is a thread-safe in-memory UserStore that also implements
// UserUpdater so both code paths in handlePatchUser can be tested.
type fakeUserStore struct {
	mu      sync.Mutex
	users   map[int64]*auth.User
	byEmail map[string]*auth.User
	nextID  int64
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:   make(map[int64]*auth.User),
		byEmail: make(map[string]*auth.User),
		nextID:  1,
	}
}

func (s *fakeUserStore) CountUsers(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.users)), nil
}

func (s *fakeUserStore) GetUserByEmail(ctx context.Context, email string) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byEmail[email]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *fakeUserStore) CreateUser(ctx context.Context, user auth.User) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byEmail[user.Email]; exists {
		return nil, auth.ErrEmailTaken
	}
	user.ID = s.nextID
	s.nextID++
	if user.CreatedAt.IsZero() {
		user.CreatedAt = time.Now().UTC()
	}
	u := user
	s.users[u.ID] = &u
	s.byEmail[u.Email] = &u
	return &u, nil
}

func (s *fakeUserStore) GetUserByID(ctx context.Context, id int64) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *fakeUserStore) BumpTokenGen(ctx context.Context, id int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return 0, auth.ErrUserNotFound
	}
	u.TokenGen++
	return u.TokenGen, nil
}

func (s *fakeUserStore) ListUsers(ctx context.Context, limit, offset int) ([]auth.User, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := int64(len(s.users))

	sorted := make([]auth.User, 0, len(s.users))
	for _, u := range s.users {
		sorted = append(sorted, *u)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	if offset >= len(sorted) {
		return []auth.User{}, total, nil
	}
	sorted = sorted[offset:]
	if limit > 0 && limit < len(sorted) {
		sorted = sorted[:limit]
	}
	return sorted, total, nil
}

func (s *fakeUserStore) UpdateUserRole(ctx context.Context, id int64, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return auth.ErrUserNotFound
	}
	u.SystemRole = role
	s.byEmail[u.Email].SystemRole = role
	return nil
}

func (s *fakeUserStore) DeleteUser(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return auth.ErrUserNotFound
	}
	delete(s.byEmail, u.Email)
	delete(s.users, id)
	return nil
}

// UpdateUserFields implements UserUpdater.
func (s *fakeUserStore) UpdateUserFields(ctx context.Context, id int64, name, passwordHash *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return auth.ErrUserNotFound
	}
	if name != nil {
		u.Name = *name
		s.byEmail[u.Email].Name = *name
	}
	if passwordHash != nil {
		u.PasswordHash = *passwordHash
		s.byEmail[u.Email].PasswordHash = *passwordHash
	}
	return nil
}

// fakeStatsCollector is a preset-data stats.Collector.
type fakeStatsCollector struct {
	byProto map[string]artifact.SizeStat
	total   artifact.SizeStat
}

func newFakeStatsCollector() *fakeStatsCollector {
	return &fakeStatsCollector{
		byProto: map[string]artifact.SizeStat{
			"oci": {Bytes: 2048, Objects: 3},
		},
		total: artifact.SizeStat{Bytes: 2048, Objects: 3},
	}
}

func (c *fakeStatsCollector) ByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error) {
	return c.byProto, nil
}
func (c *fakeStatsCollector) Total(ctx context.Context) (artifact.SizeStat, error) {
	return c.total, nil
}
func (c *fakeStatsCollector) RecordPut(_ context.Context, _ string, _ int64) error   { return nil }
func (c *fakeStatsCollector) RecordEvict(_ context.Context, _ string, _ int64) error { return nil }
func (c *fakeStatsCollector) Run(_ context.Context)                                  {}

// fakeBlobReporter implements BlobUsageReporter.
type fakeBlobReporter struct{ usedBytes int64 }

func (r *fakeBlobReporter) UsageBytes(_ context.Context) (int64, error) { return r.usedBytes, nil }

// fakeMetaStore satisfies meta.MetadataStore (used in Deps.Meta; not exercised
// by admin handlers yet but required for construction).
type fakeMetaStore struct{}

func (m *fakeMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (m *fakeMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (m *fakeMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }
func (m *fakeMetaStore) GetMutable(_ context.Context, _ string) (*artifact.MutableEntry, error) {
	return nil, nil
}
func (m *fakeMetaStore) PutMutable(_ context.Context, _ artifact.MutableEntry) error { return nil }
func (m *fakeMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

// ---- test helpers ------------------------------------------------------------

// testConfig returns a minimal *config.Config for testing.
func testConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			DataPlaneAddr:    ":5000",
			ControlPlaneAddr: ":8080",
		},
		Storage: config.StorageConfig{
			Blob: config.BlobStorageConfig{Driver: "local", Local: config.LocalBlobConfig{Root: "/tmp/test-blobs"}},
			Meta: config.MetaStorageConfig{Driver: "sqlite", DSN: "/tmp/test.db"},
		},
		Auth: config.AuthConfig{JWTSecret: "test-secret-32-bytes-minimum!!!"},
		Protocols: map[string]config.ProtocolConfig{
			"oci": {
				Upstreams: []config.UpstreamConfig{
					{Name: "dockerhub", BaseURL: "https://registry-1.docker.io", Priority: 0, Official: true},
				},
				Verification:      config.VerificationConfig{Tiers: []string{"checksum", "tofu"}},
				MutableTTLSeconds: 300,
			},
		},
	}
}

// harness holds the constructed test server plus the fakes for direct inspection.
type harness struct {
	srv      *Server
	mux      *http.ServeMux
	store    *fakeUserStore
	verifier auth.TokenVerifier
	hasher   *fakeHasher
}

// newHarness creates a complete admin server wired with fakes.
func newHarness(t *testing.T) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false)

	srv := New(Deps{
		Stats:  newFakeStatsCollector(),
		Meta:   &fakeMetaStore{},
		Users:  store,
		Auth:   svc,
		Tokens: verifier,
		Config: testConfig(),
		Blobs:  &fakeBlobReporter{usedBytes: 999},
		Secure: false,
		Logger: nil,
	})
	// Override the internal bcrypt hasher with the fast fake so tests don't
	// spend time in bcrypt. The fakeHasher satisfies auth.PasswordHasher.
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// mustCreateAdmin creates an admin user in the fake store and returns a signed
// JWT so requests to admin-only routes can be authenticated.
func (h *harness) mustCreateAdmin(t *testing.T) (auth.User, string) {
	t.Helper()
	u, err := h.store.CreateUser(context.Background(), auth.User{
		Email:        "admin@example.com",
		Name:         "Admin",
		PasswordHash: "hash:adminpass",
		SystemRole:   "admin",
		TokenGen:     0,
	})
	require.NoError(t, err)
	tok, err := h.verifier.Sign(*u)
	require.NoError(t, err)
	return *u, tok
}

// mustCreateUser creates a regular user and returns it + a JWT.
func (h *harness) mustCreateUser(t *testing.T, email string) (auth.User, string) {
	t.Helper()
	u, err := h.store.CreateUser(context.Background(), auth.User{
		Email:        email,
		Name:         "Regular",
		PasswordHash: "hash:userpass",
		SystemRole:   "user",
		TokenGen:     0,
	})
	require.NoError(t, err)
	tok, err := h.verifier.Sign(*u)
	require.NoError(t, err)
	return *u, tok
}

// do executes an HTTP request against the test mux.
// If token is non-empty it is set as the session cookie.
// If body is non-nil the Content-Type is set to application/json.
func (h *harness) do(method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.TokenCookieName, Value: token})
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.mux.ServeHTTP(rr, req)
	return rr
}

// jsonBody encodes v as JSON into an io.Reader for use as request body.
func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// decodeJSON decodes a JSON response body into dst.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(rr.Body).Decode(dst))
}

// ---- register ----------------------------------------------------------------

func TestHandleRegister(t *testing.T) {
	t.Run("success first user becomes admin", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "first@example.com",
			Password: "longpassword",
		}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp LoginResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "first@example.com", resp.User.Email)
		assert.Equal(t, "admin", resp.User.SystemRole, "first user must be admin")

		// Cookie must be set.
		cookies := rr.Result().Cookies()
		found := false
		for _, c := range cookies {
			if c.Name == auth.TokenCookieName {
				found = true
				assert.True(t, c.HttpOnly)
			}
		}
		assert.True(t, found, "session cookie must be set on register")
	})

	t.Run("name field is persisted", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "named@example.com",
			Name:     "Jane Doe",
			Password: "longpassword",
		}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp LoginResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "Jane Doe", resp.User.Name, "name provided at register must be returned in the response")

		// Verify the name is persisted in the store.
		u, err := h.store.GetUserByEmail(context.Background(), "named@example.com")
		require.NoError(t, err)
		assert.Equal(t, "Jane Doe", u.Name, "name must be stored in the user record")
	})

	t.Run("second user is regular", func(t *testing.T) {
		h := newHarness(t)
		h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "second@example.com",
			Password: "longpassword",
		}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp LoginResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "user", resp.User.SystemRole)
	})

	t.Run("invalid body", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/register", "", bytes.NewBufferString("not-json"))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("password too short", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "a@b.com",
			Password: "short",
		}))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("email already taken", func(t *testing.T) {
		h := newHarness(t)
		h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "admin@example.com",
			Password: "longpassword",
		}))
		assert.Equal(t, http.StatusConflict, rr.Code)
	})

	t.Run("empty email", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/register", "", jsonBody(RegisterRequest{
			Email:    "",
			Password: "longpassword",
		}))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// ---- login -------------------------------------------------------------------

func TestHandleLogin(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/auth/login", "", jsonBody(LoginRequest{
			Email:    "admin@example.com",
			Password: "adminpass",
		}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp LoginResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "admin@example.com", resp.User.Email)

		// Cookie must be present.
		found := false
		for _, c := range rr.Result().Cookies() {
			if c.Name == auth.TokenCookieName {
				found = true
			}
		}
		assert.True(t, found)
	})

	t.Run("wrong password", func(t *testing.T) {
		h := newHarness(t)
		h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/auth/login", "", jsonBody(LoginRequest{
			Email:    "admin@example.com",
			Password: "wrongpassword",
		}))
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("unknown email", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/login", "", jsonBody(LoginRequest{
			Email:    "nobody@example.com",
			Password: "somepass1234",
		}))
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("invalid body", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/login", "", bytes.NewBufferString("{bad"))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// ---- logout ------------------------------------------------------------------

func TestHandleLogout(t *testing.T) {
	t.Run("with valid session bumps token gen", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/auth/logout", tok, nil)
		assert.Equal(t, http.StatusNoContent, rr.Code)

		// Cookie must be cleared (MaxAge=-1 or empty).
		for _, c := range rr.Result().Cookies() {
			if c.Name == auth.TokenCookieName {
				assert.True(t, c.MaxAge < 0 || c.Value == "", "logout must clear cookie")
			}
		}

		// Verify that the old token is now rejected (token_gen bumped).
		u, err := h.store.GetUserByID(context.Background(), admin.ID)
		require.NoError(t, err)
		assert.Greater(t, u.TokenGen, admin.TokenGen, "token_gen must be incremented on logout")
	})

	t.Run("without session returns 204 and clears cookie", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("POST", "/api/v1/auth/logout", "", nil)
		assert.Equal(t, http.StatusNoContent, rr.Code)
	})
}

// ---- me ----------------------------------------------------------------------

func TestHandleMe(t *testing.T) {
	t.Run("returns current user", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		rr := h.do("GET", "/api/v1/me", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, admin.ID, resp.User.ID)
		assert.Equal(t, admin.Email, resp.User.Email)
	})

	t.Run("no session returns 401", func(t *testing.T) {
		h := newHarness(t)
		rr := h.do("GET", "/api/v1/me", "", nil)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

// ---- stats -------------------------------------------------------------------

func TestHandleStats(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp StatsResponse
	decodeJSON(t, rr, &resp)
	assert.Equal(t, int64(2048), resp.TotalBytes)
	assert.Equal(t, int64(3), resp.TotalObjects)
	assert.Equal(t, int64(999), resp.BackendDiskUsed)
	require.Len(t, resp.PerProtocol, 1)
	assert.Equal(t, "oci", resp.PerProtocol[0].Protocol)
	assert.Equal(t, int64(2048), resp.PerProtocol[0].Bytes)
}

func TestHandleStatsSeries(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	t.Run("grand total when protocol omitted", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats/series", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp SeriesResponse
		decodeJSON(t, rr, &resp)
		assert.Empty(t, resp.Protocol)
		require.Len(t, resp.Points, 1)
		assert.Equal(t, int64(2048), resp.Points[0].Bytes)
	})

	t.Run("per-protocol when protocol given", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats/series?protocol=oci", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp SeriesResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "oci", resp.Protocol)
		require.Len(t, resp.Points, 1)
		assert.Equal(t, int64(2048), resp.Points[0].Bytes)
	})

	t.Run("unknown protocol returns 0 bytes", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats/series?protocol=pypi", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp SeriesResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "pypi", resp.Protocol)
		require.Len(t, resp.Points, 1)
		assert.Equal(t, int64(0), resp.Points[0].Bytes)
	})
}

// ---- upstreams ---------------------------------------------------------------

func TestHandleUpstreams(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/upstreams", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp UpstreamsResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Upstreams, 1)
	assert.Equal(t, "oci", resp.Upstreams[0].Protocol)
	assert.Equal(t, "https://registry-1.docker.io", resp.Upstreams[0].URL)
	assert.False(t, resp.Upstreams[0].Blocked)
}

// ---- list users --------------------------------------------------------------

func TestHandleListUsers(t *testing.T) {
	h := newHarness(t)
	admin, tok := h.mustCreateAdmin(t)
	h.mustCreateUser(t, "user1@example.com")
	h.mustCreateUser(t, "user2@example.com")

	t.Run("returns all users", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/users", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp UsersResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, int64(3), resp.Total)
		assert.Len(t, resp.Users, 3)
	})

	t.Run("limit and offset", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/users?limit=1&offset=1", tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp UsersResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, int64(3), resp.Total)
		assert.Len(t, resp.Users, 1)
		_ = admin // suppress unused warning
	})

	t.Run("non-admin gets 403", func(t *testing.T) {
		_, userTok := h.mustCreateUser(t, "other@example.com")
		rr := h.do("GET", "/api/v1/admin/users", userTok, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})
}

// ---- create user -------------------------------------------------------------

func TestHandleCreateUser(t *testing.T) {
	t.Run("success with explicit role", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Email:      "new@example.com",
			Name:       "New User",
			Password:   "strongpassword",
			SystemRole: "user",
		}))
		assert.Equal(t, http.StatusCreated, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "new@example.com", dto.Email)
		assert.Equal(t, "New User", dto.Name)
		assert.Equal(t, "user", dto.SystemRole)
		assert.NotZero(t, dto.ID)
	})

	t.Run("defaults system_role to user when omitted", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Email:    "norole@example.com",
			Password: "strongpassword",
		}))
		assert.Equal(t, http.StatusCreated, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "user", dto.SystemRole)
	})

	t.Run("normalises email to lower case", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Email:    "UPPER@EXAMPLE.COM",
			Password: "strongpassword",
		}))
		assert.Equal(t, http.StatusCreated, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "upper@example.com", dto.Email)
	})

	t.Run("missing email returns 400", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Password: "strongpassword",
		}))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("short password returns 400", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Email:    "short@example.com",
			Password: "abc",
		}))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("duplicate email returns 409", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("POST", "/api/v1/admin/users", tok, jsonBody(CreateUserRequest{
			Email:    "admin@example.com",
			Password: "strongpassword",
		}))
		assert.Equal(t, http.StatusConflict, rr.Code)
	})
}

// ---- get user ----------------------------------------------------------------

func TestHandleGetUser(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		rr := h.do("GET", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, admin.ID, dto.ID)
		assert.Equal(t, admin.Email, dto.Email)
	})

	t.Run("not found returns 404", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("GET", "/api/v1/admin/users/9999", tok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("invalid id returns 400", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)

		rr := h.do("GET", "/api/v1/admin/users/notanumber", tok, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// ---- patch user --------------------------------------------------------------

func TestHandlePatchUser(t *testing.T) {
	t.Run("update role succeeds", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		user, _ := h.mustCreateUser(t, "patchme@example.com")

		newRole := "admin"
		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), tok,
			jsonBody(PatchUserRequest{SystemRole: &newRole}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "admin", dto.SystemRole)
	})

	t.Run("update name via UserUpdater", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		user, _ := h.mustCreateUser(t, "named@example.com")

		newName := "Updated Name"
		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), tok,
			jsonBody(PatchUserRequest{Name: &newName}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "Updated Name", dto.Name)
	})

	t.Run("update password via UserUpdater", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		user, _ := h.mustCreateUser(t, "pwchange@example.com")

		newPw := "newlongpassword"
		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), tok,
			jsonBody(PatchUserRequest{Password: &newPw}))
		assert.Equal(t, http.StatusOK, rr.Code)

		// Verify the password hash was updated in the store.
		stored, err := h.store.GetUserByID(context.Background(), user.ID)
		require.NoError(t, err)
		assert.Equal(t, "hash:newlongpassword", stored.PasswordHash)
	})

	t.Run("cannot demote last admin", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		newRole := "user"
		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok,
			jsonBody(PatchUserRequest{SystemRole: &newRole}))
		assert.Equal(t, http.StatusConflict, rr.Code)
	})

	t.Run("can demote admin when another admin exists", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)
		// Add a second admin.
		second, _ := h.mustCreateUser(t, "second@example.com")
		promote := "admin"
		require.NoError(t, h.store.UpdateUserRole(context.Background(), second.ID, promote))

		newRole := "user"
		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok,
			jsonBody(PatchUserRequest{SystemRole: &newRole}))
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("not found returns 404", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		newRole := "user"
		rr := h.do("PATCH", "/api/v1/admin/users/9999", tok,
			jsonBody(PatchUserRequest{SystemRole: &newRole}))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("noop patch returns current user", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok,
			jsonBody(PatchUserRequest{}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var dto UserDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, admin.ID, dto.ID)
	})
}

// ---- delete user -------------------------------------------------------------

func TestHandleDeleteUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		// Add a second admin so first admin can delete the regular user without
		// hitting the last-admin guard.
		user, _ := h.mustCreateUser(t, "todelete@example.com")

		// Add second admin so first admin can delete a user.
		second, _ := h.mustCreateUser(t, "second@example.com")
		require.NoError(t, h.store.UpdateUserRole(context.Background(), second.ID, "admin"))

		rr := h.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), tok, nil)
		assert.Equal(t, http.StatusNoContent, rr.Code)

		_, err := h.store.GetUserByID(context.Background(), user.ID)
		assert.ErrorIs(t, err, auth.ErrUserNotFound)
	})

	t.Run("cannot delete self", func(t *testing.T) {
		h := newHarness(t)
		admin, tok := h.mustCreateAdmin(t)

		rr := h.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok, nil)
		assert.Equal(t, http.StatusConflict, rr.Code)
	})

	t.Run("cannot delete last admin", func(t *testing.T) {
		// Scenario: two admins exist; one deletes the other, leaving themselves
		// as the last admin. After that deletion, a further delete of the last
		// admin hits the self-delete guard (which fires before the last-admin
		// guard since the caller IS the last admin).
		h := newHarness(t)
		adminUser, adminTok := h.mustCreateAdmin(t)
		regularUser, _ := h.mustCreateUser(t, "regular@example.com")
		require.NoError(t, h.store.UpdateUserRole(context.Background(), regularUser.ID, "admin"))

		// Delete regularUser-turned-admin → now adminUser is the only admin.
		delRR := h.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(regularUser.ID, 10), adminTok, nil)
		assert.Equal(t, http.StatusNoContent, delRR.Code)

		// Now adminUser is the last admin; trying to self-delete hits the
		// self-delete guard (which takes priority over last-admin guard).
		delRR2 := h.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(adminUser.ID, 10), adminTok, nil)
		assert.Equal(t, http.StatusConflict, delRR2.Code)

		// Verify the last-admin guard specifically: create a second admin and
		// use it to try deleting adminUser (last remaining admin after
		// second-admin deletes itself — but that's self-delete). Instead,
		// second admin tries to delete adminUser when adminUser IS the last admin.
		h2 := newHarness(t)
		lastAdmin, _ := h2.mustCreateAdmin(t)
		secondAdmin, secondAdminTok := h2.mustCreateUser(t, "second@example.com")
		require.NoError(t, h2.store.UpdateUserRole(context.Background(), secondAdmin.ID, "admin"))
		// Delete secondAdmin so lastAdmin becomes the only admin.
		delRR3 := h2.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(secondAdmin.ID, 10), secondAdminTok, nil)
		// secondAdmin is deleting themselves → self-delete guard fires.
		assert.Equal(t, http.StatusConflict, delRR3.Code)
		// Use a fresh third admin to delete secondAdmin legitimately.
		thirdAdmin, thirdAdminTok := h2.mustCreateUser(t, "third@example.com")
		require.NoError(t, h2.store.UpdateUserRole(context.Background(), thirdAdmin.ID, "admin"))
		delRR4 := h2.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(secondAdmin.ID, 10), thirdAdminTok, nil)
		assert.Equal(t, http.StatusNoContent, delRR4.Code)
		// Now lastAdmin and thirdAdmin remain. Delete thirdAdmin to make lastAdmin the only admin.
		delRR5 := h2.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(thirdAdmin.ID, 10), thirdAdminTok, nil)
		// thirdAdmin trying to delete themselves → self-delete guard.
		assert.Equal(t, http.StatusConflict, delRR5.Code)
		// Use lastAdmin to delete thirdAdmin.
		lastAdminTok, err := h2.verifier.Sign(lastAdmin)
		require.NoError(t, err)
		delRR6 := h2.do("DELETE", "/api/v1/admin/users/"+strconv.FormatInt(thirdAdmin.ID, 10), lastAdminTok, nil)
		assert.Equal(t, http.StatusNoContent, delRR6.Code)
		// Now lastAdmin is the truly last admin. Any other user (non-admin) can't
		// delete them (403). Verify last-admin guard via a direct isLastAdmin call.
		isLast, err := h2.srv.isLastAdmin(context.Background(), lastAdmin.ID)
		require.NoError(t, err)
		assert.True(t, isLast, "lastAdmin should be recognised as the last admin")
	})

	t.Run("not found returns 404", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		// Add second admin so we don't hit self-delete on first admin.
		second, _ := h.mustCreateUser(t, "second@example.com")
		require.NoError(t, h.store.UpdateUserRole(context.Background(), second.ID, "admin"))

		rr := h.do("DELETE", "/api/v1/admin/users/9999", tok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("invalid id returns 400", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		rr := h.do("DELETE", "/api/v1/admin/users/abc", tok, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// ---- config ------------------------------------------------------------------

func TestHandleConfig(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/config", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp ConfigResponse
	decodeJSON(t, rr, &resp)
	assert.Equal(t, ":5000", resp.DataPlaneAddr)
	assert.Equal(t, ":8080", resp.ControlPlaneAddr)
	assert.Equal(t, "local", resp.BlobDriver)
	assert.Equal(t, "sqlite", resp.MetaDriver)
	require.Len(t, resp.Protocols, 1)
	assert.Equal(t, "oci", resp.Protocols[0].Protocol)
	require.Len(t, resp.Protocols[0].Upstreams, 1)
	assert.Equal(t, "dockerhub", resp.Protocols[0].Upstreams[0].Name)

	// Secrets must never appear in the response body.
	raw := rr.Body.String()
	assert.NotContains(t, raw, "jwt_secret", "jwt_secret must be redacted")
	assert.NotContains(t, raw, "test-secret", "JWT secret value must be redacted")
	assert.NotContains(t, raw, "admin_key", "admin_key must be redacted")
}

// ---- events ------------------------------------------------------------------

func TestHandleEvents(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/events", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp EventsResponse
	decodeJSON(t, rr, &resp)
	assert.NotNil(t, resp.Events)
	assert.Empty(t, resp.Events)
}

// ---- auth middleware guards --------------------------------------------------

func TestAuthMiddlewareGuards(t *testing.T) {
	h := newHarness(t)
	admin, adminTok := h.mustCreateAdmin(t)
	_, userTok := h.mustCreateUser(t, "plain@example.com")

	adminRoutes := []struct{ method, path string }{
		{"GET", "/api/v1/admin/stats"},
		{"GET", "/api/v1/admin/stats/series"},
		{"GET", "/api/v1/admin/upstreams"},
		{"GET", "/api/v1/admin/users"},
		{"GET", "/api/v1/admin/users/" + strconv.FormatInt(admin.ID, 10)},
		{"GET", "/api/v1/admin/config"},
		{"GET", "/api/v1/admin/events"},
	}

	for _, tc := range adminRoutes {
		tc := tc
		t.Run(tc.method+" "+tc.path+" no-session → 401", func(t *testing.T) {
			rr := h.do(tc.method, tc.path, "", nil)
			assert.Equal(t, http.StatusUnauthorized, rr.Code)
		})
		t.Run(tc.method+" "+tc.path+" non-admin → 403", func(t *testing.T) {
			rr := h.do(tc.method, tc.path, userTok, nil)
			assert.Equal(t, http.StatusForbidden, rr.Code)
		})
		t.Run(tc.method+" "+tc.path+" admin → not 401/403", func(t *testing.T) {
			rr := h.do(tc.method, tc.path, adminTok, nil)
			assert.NotEqual(t, http.StatusUnauthorized, rr.Code)
			assert.NotEqual(t, http.StatusForbidden, rr.Code)
		})
	}
}

// ---- revocation (token_gen) --------------------------------------------------

func TestTokenGenRevocation(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	// Token works before logout.
	rr := h.do("GET", "/api/v1/me", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	// Logout bumps token_gen.
	rr = h.do("POST", "/api/v1/auth/logout", tok, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Old token must now be rejected by the middleware.
	rr = h.do("GET", "/api/v1/me", tok, nil)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

// ---- JSON field shapes -------------------------------------------------------

func TestJSONFieldNames(t *testing.T) {
	// Ensure the wire contract JSON field names are stable.
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	t.Run("StatsResponse field names", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var raw map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&raw))
		for _, key := range []string{"per_protocol", "total_bytes", "total_objects", "backend_disk_free", "backend_disk_used"} {
			assert.Contains(t, raw, key, "missing JSON field %q", key)
		}
	})

	t.Run("UserDTO field names", func(t *testing.T) {
		// Use the existing admin (already in the store; avoid duplicate email).
		admin, err := h.store.GetUserByEmail(context.Background(), "admin@example.com")
		require.NoError(t, err)
		rr := h.do("GET", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok, nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var raw map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&raw))
		for _, key := range []string{"id", "email", "name", "system_role", "created_at"} {
			assert.Contains(t, raw, key, "missing JSON field %q", key)
		}
	})

	t.Run("SeriesResponse field names", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats/series", tok, nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var raw map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&raw))
		assert.Contains(t, raw, "protocol")
		assert.Contains(t, raw, "points")
	})

	// Ensure series points have correct field names.
	t.Run("SeriesPoint field names", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/stats/series", tok, nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var resp struct {
			Points []map[string]json.RawMessage `json:"points"`
		}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		require.Len(t, resp.Points, 1)
		assert.Contains(t, resp.Points[0], "unix")
		assert.Contains(t, resp.Points[0], "bytes")
	})
}

// ---- timestamp encoding (RFC3339 / Unix) ------------------------------------

func TestTimestampEncoding(t *testing.T) {
	h := newHarness(t)
	admin, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/users/"+strconv.FormatInt(admin.ID, 10), tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	// created_at must be an RFC3339 string, not a Unix integer.
	var raw map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&raw))
	var ts time.Time
	require.NoError(t, json.Unmarshal(raw["created_at"], &ts), "created_at must parse as time.Time/RFC3339")
	assert.False(t, ts.IsZero())
}

package admin

// missing_coverage_test.go fills the coverage gaps identified by per-function
// profiling at the baseline of 69.3%.
//
// Does NOT duplicate tests in:
//   handlers_test.go, org_ported_test.go, repos_test.go, settings_test.go,
//   cache_test.go, org_quota_test.go
//
// Gap order (highest impact first):
//   handleGetOrg (0%), handleInstance/registryHost (0%), handleAcceptInvitation (0%)
//   inviteRole branches (22%), handlePatchMember (38%), handleLeaveOrg (47%)
//   handleUpdateOrg (51%), handlePatchUser (52%), handleRemoveMember (56%)
//   handleStats/Series error paths (65-66%), handleListOrgs API-key branch (61%)
//   key store nil and error paths (60-66%), canonicalProtocol "go"→"gomod" (66%)
//   handlePatchInvitation invalid status (60%), guardLastPrivileged 503 (68%)

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/stats"
)

// ── error-injecting test doubles ─────────────────────────────────────────────

// errStatsCollector satisfies stats.Collector with configurable error returns
// on ByProtocol / Total. Used to prove handleStats and handleStatsSeries
// return 500 under a store fault, never 200.
type errStatsCollector struct {
	byProtoErr error
	totalErr   error
}

func (c *errStatsCollector) ByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, c.byProtoErr
}
func (c *errStatsCollector) Total(_ context.Context) (artifact.SizeStat, error) {
	return artifact.SizeStat{}, c.totalErr
}
func (c *errStatsCollector) RecordPut(_ context.Context, _ string, _ int64) error   { return nil }
func (c *errStatsCollector) RecordEvict(_ context.Context, _ string, _ int64) error { return nil }
func (c *errStatsCollector) EvictionTotals() (int64, int64)                         { return 0, 0 }
func (c *errStatsCollector) Refresh(_ context.Context)                              {}
func (c *errStatsCollector) Run(_ context.Context)                                  {}
func (c *errStatsCollector) AddOpaquePath(_, _ string)                              {}
func (c *errStatsCollector) Series(_ context.Context, _ string) ([]stats.SeriesPoint, error) {
	return nil, nil
}

// errOrgStore wraps fakeOrgStore and injects errors on CountOrgOwners /
// CountOrgAdmins so that guardLastPrivileged's fail-CLOSED 503 branch can be
// exercised. A guard that disables itself under DB trouble is exactly when the
// last owner gets removed.
type errOrgStore struct {
	*fakeOrgStore
	countOwnersErr error
	countAdminsErr error
}

func (e *errOrgStore) CountOrgOwners(ctx context.Context, orgID string) (int, error) {
	if e.countOwnersErr != nil {
		return 0, e.countOwnersErr
	}
	return e.fakeOrgStore.CountOrgOwners(ctx, orgID)
}

func (e *errOrgStore) CountOrgAdmins(ctx context.Context, orgID string) (int, error) {
	if e.countAdminsErr != nil {
		return 0, e.countAdminsErr
	}
	return e.fakeOrgStore.CountOrgAdmins(ctx, orgID)
}

// errKeyStore wraps fakeAPIKeyStore and returns a configurable error on List.
type errKeyStore struct {
	*fakeAPIKeyStore
	listErr error
}

func (e *errKeyStore) List(orgID string) ([]apikey.KeyInfo, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.fakeAPIKeyStore.List(orgID)
}

// listOrgsErrOrgStore wraps fakeOrgStore and fails ListOrgsForEmail.
type listOrgsErrOrgStore struct {
	*fakeOrgStore
}

func (e *listOrgsErrOrgStore) ListOrgsForEmail(_ context.Context, _ string) ([]*org.Org, error) {
	return nil, errors.New("db unavailable")
}

// ── harness constructors ──────────────────────────────────────────────────────

func newHarnessWithErrStats(t *testing.T, sc *errStatsCollector) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:  sc,
		Meta:   &fakeMetaStore{},
		Users:  store,
		Auth:   svc,
		Tokens: verifier,
		Config: testConfig(),
		Blobs:  &fakeBlobReporter{usedBytes: 1},
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// newHarnessWithErrOrg builds a harness using the given errOrgStore instead
// of the normal fakeOrgStore, so CountOrgOwners/CountOrgAdmins can fail.
func newHarnessWithErrOrg(t *testing.T, eos *errOrgStore) (*harness, *fakeAPIKeyStore) {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	keyStore := newFakeAPIKeyStore()
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:    newFakeStatsCollector(),
		Meta:     &fakeMetaStore{},
		Users:    store,
		Auth:     svc,
		Tokens:   verifier,
		Config:   testConfig(),
		Blobs:    &fakeBlobReporter{usedBytes: 1},
		OrgStore: eos,
		KeyStore: keyStore,
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}, keyStore
}

// newHarnessWithErrKeys builds a MT harness where the key-store List call
// returns a configurable error, so the handleListKeys 500 path can be tested.
func newHarnessWithErrKeys(t *testing.T, ek *errKeyStore) (*harness, *fakeOrgStore) {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	orgStore := newFakeOrgStore()
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:    newFakeStatsCollector(),
		Meta:     &fakeMetaStore{},
		Users:    store,
		Auth:     svc,
		Tokens:   verifier,
		Config:   testConfig(),
		Blobs:    &fakeBlobReporter{usedBytes: 1},
		OrgStore: orgStore,
		KeyStore: ek,
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}, orgStore
}

// newHarnessWithListOrgsErr builds a MT harness whose org store fails
// ListOrgsForEmail, so handleListOrgs returns 500 on store faults.
func newHarnessWithListOrgsErr(t *testing.T) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	eos := &listOrgsErrOrgStore{fakeOrgStore: newFakeOrgStore()}
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:    newFakeStatsCollector(),
		Meta:     &fakeMetaStore{},
		Users:    store,
		Auth:     svc,
		Tokens:   verifier,
		Config:   testConfig(),
		Blobs:    &fakeBlobReporter{usedBytes: 1},
		OrgStore: eos,
		KeyStore: newFakeAPIKeyStore(),
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// newHarnessWithCustomCfg builds a plain harness with a custom *config.Config,
// allowing coverage of registryHost branches that depend on specific cfg fields.
func newHarnessWithCustomCfg(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:  newFakeStatsCollector(),
		Meta:   &fakeMetaStore{},
		Users:  store,
		Auth:   svc,
		Tokens: verifier,
		Config: cfg,
		Blobs:  &fakeBlobReporter{usedBytes: 1},
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// doRaw sends an unauthenticated HTTP request against mux with an explicit Host
// header. Useful for public endpoints like GET /api/v1/instance.
func doRaw(mux *http.ServeMux, method, path, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if host != "" {
		req.Host = host
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// strPtr returns a pointer to s for use in struct literals with *string fields.
func strPtr(s string) *string { return &s }

// ════════════════════════════════════════════════════════════════════════════
// GET /api/v1/instance — handleInstance + registryHost (0% coverage)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleInstance covers the public endpoint and all registryHost branches.
// The endpoint is deliberately unauthenticated — the WebUI needs the registry
// address before a session exists (especially for the docker login prompt).
func TestHandleInstance(t *testing.T) {
	t.Run("default config derives registry host from DataPlaneAddr port", func(t *testing.T) {
		// testConfig() uses DataPlaneAddr=":5000". When the incoming request host
		// is control.example.com:8080 the derived registry host must keep the same
		// hostname and swap in port 5000 from the data-plane address.
		h := newHarness(t)
		rr := doRaw(h.mux, "GET", "/api/v1/instance", "control.example.com:8080")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp InstanceResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "control.example.com:5000", resp.RegistryHost,
			"registryHost must swap the data-plane port in while preserving the hostname")
	})

	t.Run("RegistryPublicHost takes precedence over derivation", func(t *testing.T) {
		cfg := testConfig()
		cfg.Server.RegistryPublicHost = "registry.example.com:5001"
		h := newHarnessWithCustomCfg(t, cfg)
		rr := doRaw(h.mux, "GET", "/api/v1/instance", "control.example.com:8080")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp InstanceResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "registry.example.com:5001", resp.RegistryHost,
			"a configured RegistryPublicHost must be returned verbatim")
	})

	t.Run("nil config returns request Host verbatim", func(t *testing.T) {
		// cfg=nil exercises the `if s.cfg == nil { return r.Host }` branch.
		h := newHarnessWithCustomCfg(t, nil)
		rr := doRaw(h.mux, "GET", "/api/v1/instance", "localhost:9999")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp InstanceResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "localhost:9999", resp.RegistryHost,
			"nil config must return the request Host unchanged")
	})

	t.Run("empty DataPlaneAddr returns request Host unchanged", func(t *testing.T) {
		// An empty DataPlaneAddr produces an empty port → fallback to r.Host.
		cfg := testConfig()
		cfg.Server.DataPlaneAddr = ""
		h := newHarnessWithCustomCfg(t, cfg)
		rr := doRaw(h.mux, "GET", "/api/v1/instance", "specula.example.com:8080")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp InstanceResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "specula.example.com:8080", resp.RegistryHost,
			"empty DataPlaneAddr must fall back to the request Host")
	})

	t.Run("host without port gets data-plane port appended", func(t *testing.T) {
		// When the incoming Host has no port, net.SplitHostPort returns an error,
		// host stays as-is, and the data-plane port is appended via JoinHostPort.
		h := newHarness(t) // DataPlaneAddr=":5000"
		rr := doRaw(h.mux, "GET", "/api/v1/instance", "bare-host")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp InstanceResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "bare-host:5000", resp.RegistryHost,
			"a bare hostname with no port must get the data-plane port appended")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// GET /api/v1/orgs/{id} — handleGetOrg (0% coverage)
// ════════════════════════════════════════════════════════════════════════════

func TestHandleGetOrg(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*harness, *fakeOrgStore) {
		t.Helper()
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org1", Name: "Test Org", Slug: "test-org", Status: org.StatusActive,
		})
		return h, orgStore
	}

	t.Run("member reads org successfully", func(t *testing.T) {
		h, orgStore := setup(t)
		member, memberTok := h.mustCreateUser(t, "member@example.com")
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org1", Email: member.Email, Role: org.RoleViewer,
		})

		rr := h.do("GET", "/api/v1/orgs/org1", memberTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)
		var dto OrgDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "org1", dto.ID)
		assert.Equal(t, "Test Org", dto.Name)
	})

	t.Run("system admin reads org without membership (bypass)", func(t *testing.T) {
		h, _ := setup(t)
		_, adminTok := h.mustCreateAdmin(t)
		// Admin is NOT added as a member of org1.
		rr := h.do("GET", "/api/v1/orgs/org1", adminTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code,
			"system admin must bypass the membership check")
	})

	t.Run("non-member is denied with 403", func(t *testing.T) {
		h, _ := setup(t)
		_, outsiderTok := h.mustCreateUser(t, "outsider@example.com")
		rr := h.do("GET", "/api/v1/orgs/org1", outsiderTok, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"a user who is not a member must not see org details")
	})

	t.Run("unknown org returns 404", func(t *testing.T) {
		h, _ := setup(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("GET", "/api/v1/orgs/does-not-exist", adminTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("no org store returns 501", func(t *testing.T) {
		// Plain harness has no OrgStore.
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("GET", "/api/v1/orgs/org1", adminTok, nil)
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		h, _ := setup(t)
		rr := h.do("GET", "/api/v1/orgs/org1", "", nil)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

// ════════════════════════════════════════════════════════════════════════════
// POST /api/v1/invitations/accept — handleAcceptInvitation (0% coverage)
// ════════════════════════════════════════════════════════════════════════════

func TestHandleAcceptInvitationAlias(t *testing.T) {
	ctx := context.Background()

	t.Run("no org store returns 501", func(t *testing.T) {
		h := newHarness(t)
		_, tok := h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/invitations/accept", tok,
			jsonBody(AcceptInvitationRequest{Token: "any-token"}))
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})

	t.Run("bad body returns 400", func(t *testing.T) {
		h, _, _ := newHarnessWithMT(t)
		_, tok := h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/invitations/accept", tok,
			bytes.NewBufferString("{bad-json"))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("valid token accepted via alias endpoint", func(t *testing.T) {
		h, orgStore, _ := newHarnessWithMT(t)

		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-accept-alias", Name: "Accept Alias", Slug: "accept-alias",
			Status: org.StatusActive,
		})
		invitee, inviteeTok := h.mustCreateUser(t, "invitee-alias@example.com")

		inv := &org.Invitation{
			OrgID:     "org-accept-alias",
			Email:     invitee.Email,
			Role:      org.RoleViewer,
			Token:     "alias-accept-token",
			Status:    org.InviteStatusPending,
			ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}
		_ = orgStore.CreateInvitation(ctx, inv)

		rr := h.do("POST", "/api/v1/invitations/accept", inviteeTok,
			jsonBody(AcceptInvitationRequest{Token: "alias-accept-token"}))
		assert.Equal(t, http.StatusOK, rr.Code,
			"the accept-alias endpoint must create membership and return 200")

		// Membership row must exist.
		m, err := orgStore.GetOrgMember(ctx, "org-accept-alias", invitee.Email)
		require.NoError(t, err, "acceptance via alias must write a membership row")
		assert.Equal(t, org.RoleViewer, m.Role)
	})
}

// ════════════════════════════════════════════════════════════════════════════
// inviteRole validation (22.2% → missing: owner/invalid branches)
// ════════════════════════════════════════════════════════════════════════════

// TestInviteRoleValidation covers the role validation paths in inviteRole
// that are not exercised by the existing invitation lifecycle tests.
func TestInviteRoleValidation(t *testing.T) {
	ctx := context.Background()
	h, orgStore, _ := newHarnessWithMT(t)

	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org-inv-role", Name: "Invite Role", Slug: "invite-role", Status: org.StatusActive,
	})
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.AddOrgMember(ctx, &org.Member{
		OrgID: "org-inv-role", Email: adminUser.Email, Role: org.RoleOwner,
	})

	t.Run("role=owner is forbidden via invitation (403)", func(t *testing.T) {
		// Ownership must be granted by a sitting owner through the member endpoint,
		// not via an invitation token that any recipient could accept.
		rr := h.do("POST", "/api/v1/orgs/org-inv-role/invitations", adminTok,
			jsonBody(CreateInvitationRequest{
				Email: "fresh@example.com",
				Role:  "owner",
			}))
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"owner role must not be grantable via invitation")
	})

	t.Run("unrecognised role returns 400", func(t *testing.T) {
		// A typo'd role must be rejected outright, not silently downgraded to
		// viewer — the operator would see an unexpected role in the member list.
		rr := h.do("POST", "/api/v1/orgs/org-inv-role/invitations", adminTok,
			jsonBody(CreateInvitationRequest{
				Email: "fresh@example.com",
				Role:  "superuser",
			}))
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"an unrecognised role must be rejected with 400, not silently downgraded")
	})

	t.Run("empty role defaults to viewer (least privilege)", func(t *testing.T) {
		rr := h.do("POST", "/api/v1/orgs/org-inv-role/invitations", adminTok,
			jsonBody(CreateInvitationRequest{
				Email: "least-priv@example.com",
				Role:  "",
			}))
		assert.Equal(t, http.StatusCreated, rr.Code)
		var dto InvitationDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, org.RoleViewer, dto.Role,
			"omitting the role must produce the least-privilege default (viewer)")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// PATCH /api/v1/orgs/{id}/members/{email} — handlePatchMember (38.8%)
// ════════════════════════════════════════════════════════════════════════════

func TestHandlePatchMemberGaps(t *testing.T) {
	ctx := context.Background()

	// Bootstrap shared org + admin (system admin bypasses requireOrgAdmin).
	setupAdminOrg := func(t *testing.T) (*harness, *fakeOrgStore, string) {
		t.Helper()
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-pm", Name: "PatchMember", Slug: "pm", Status: org.StatusActive,
		})
		_, adminTok := h.mustCreateAdmin(t)
		return h, orgStore, adminTok
	}

	t.Run("nil role for non-existent member returns 404", func(t *testing.T) {
		h, _, adminTok := setupAdminOrg(t)
		// PatchMemberRequest with nil Role → handler returns current state, but
		// the member does not exist → 404.
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/nobody@example.com", adminTok,
			jsonBody(PatchMemberRequest{})) // Role is nil
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"nil-role PATCH for a non-existent member must return 404")
	})

	t.Run("nil role noop returns current member state", func(t *testing.T) {
		h, orgStore, adminTok := setupAdminOrg(t)
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: "noop@example.com", Role: org.RoleEditor,
		})
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/noop@example.com", adminTok,
			jsonBody(PatchMemberRequest{})) // nil Role
		assert.Equal(t, http.StatusOK, rr.Code)
		var dto MemberDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, org.RoleEditor, dto.Role,
			"nil-role noop must return the current role unchanged")
	})

	t.Run("non-existent member with non-nil role returns 404", func(t *testing.T) {
		h, _, adminTok := setupAdminOrg(t)
		editorRole := org.RoleEditor
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/ghost@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &editorRole}))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("invalid role string returns 400", func(t *testing.T) {
		h, orgStore, adminTok := setupAdminOrg(t)
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: "validmember@example.com", Role: org.RoleViewer,
		})
		badRole := "superuser"
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/validmember@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &badRole}))
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"an unrecognised role must be rejected with 400, not stored as garbage")
	})

	t.Run("granting owner requires the caller to be a sitting owner (403)", func(t *testing.T) {
		// Caller: org-admin (not owner, not system admin).
		// Granting the owner role is ownership-sensitive and is owner-only.
		h, orgStore, _ := setupAdminOrg(t)
		caller, callerTok := h.mustCreateUser(t, "orgadmin-pm@example.com")
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: caller.Email, Role: org.RoleAdmin,
		})
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: "target-pm@example.com", Role: org.RoleViewer,
		})

		ownerRole := org.RoleOwner
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/target-pm@example.com", callerTok,
			jsonBody(PatchMemberRequest{Role: &ownerRole}))
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"granting the owner role must require the caller to be a sitting owner")
	})

	t.Run("demoting a sitting owner requires the caller to be an owner (403)", func(t *testing.T) {
		// Caller: org-admin; target: current owner.
		// Demoting an owner is ownership-sensitive even if newRole < owner.
		h, orgStore, _ := setupAdminOrg(t)
		caller, callerTok := h.mustCreateUser(t, "orgadmin-pm2@example.com")
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: caller.Email, Role: org.RoleAdmin,
		})
		// Seed two owners so last-owner guard does not fire before the 403 check.
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: "owner1@example.com", Role: org.RoleOwner,
		})
		_ = orgStore.AddOrgMember(ctx, &org.Member{
			OrgID: "org-pm", Email: "owner2@example.com", Role: org.RoleOwner,
		})

		adminRole := org.RoleAdmin
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/owner1@example.com", callerTok,
			jsonBody(PatchMemberRequest{Role: &adminRole}))
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"demoting a sitting owner must require the caller to be an owner")
	})

	t.Run("no org store returns 501", func(t *testing.T) {
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		newRole := org.RoleEditor
		rr := h.do("PATCH", "/api/v1/orgs/org-pm/members/target@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &newRole}))
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})
}

// ════════════════════════════════════════════════════════════════════════════
// DELETE /api/v1/orgs/{id}/members/me — handleLeaveOrg (47.8%)
// ════════════════════════════════════════════════════════════════════════════

func TestHandleLeaveOrgGaps(t *testing.T) {
	ctx := context.Background()

	t.Run("API key caller is forbidden (403)", func(t *testing.T) {
		// handleLeaveOrg requires a human login: an API key has no email to be a
		// member under, and a machine leaving on a human's behalf would bypass the
		// last-owner guard.
		h, orgStore, keyStore := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-leave", Name: "Leave Org", Slug: "leave-org", Status: org.StatusActive,
		})
		_, rawKey, _ := keyStore.Create("org-leave", "test-leave-key")

		rr := h.doWithBearer("DELETE", "/api/v1/orgs/org-leave/members/me", rawKey, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"an API key must not be allowed to leave an org on a human's behalf")
	})

	t.Run("user not a member returns 404", func(t *testing.T) {
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-leave2", Name: "Leave Org 2", Slug: "leave-org-2", Status: org.StatusActive,
		})
		_, nonMemberTok := h.mustCreateUser(t, "notinorg@example.com")
		rr := h.do("DELETE", "/api/v1/orgs/org-leave2/members/me", nonMemberTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"a user who is not a member must get 404, not 403 or 204")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// PUT /api/v1/orgs/{id} — handleUpdateOrg (51.9%)
// ════════════════════════════════════════════════════════════════════════════

func TestHandleUpdateOrgGaps(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T, orgID string) (*harness, *fakeOrgStore, string) {
		t.Helper()
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: orgID, Name: "Update Org", Slug: orgID, Status: org.StatusActive,
		})
		_, adminTok := h.mustCreateAdmin(t)
		return h, orgStore, adminTok
	}

	t.Run("empty name returns 400", func(t *testing.T) {
		h, _, adminTok := setup(t, "org-upd1")
		rr := h.do("PUT", "/api/v1/orgs/org-upd1", adminTok,
			jsonBody(UpdateOrgRequest{Name: strPtr("")}))
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"setting an empty name must be rejected with 400")
	})

	t.Run("whitespace-only name returns 400", func(t *testing.T) {
		h, _, adminTok := setup(t, "org-upd2")
		rr := h.do("PUT", "/api/v1/orgs/org-upd2", adminTok,
			jsonBody(UpdateOrgRequest{Name: strPtr("   ")}))
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"a whitespace-only name must be rejected after trimming")
	})

	t.Run("unknown org returns 404", func(t *testing.T) {
		h, _, adminTok := setup(t, "org-upd3")
		rr := h.do("PUT", "/api/v1/orgs/does-not-exist", adminTok,
			jsonBody(UpdateOrgRequest{Name: strPtr("New Name")}))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("no org store returns 501", func(t *testing.T) {
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("PUT", "/api/v1/orgs/org-upd", adminTok,
			jsonBody(UpdateOrgRequest{Name: strPtr("Updated")}))
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})

	t.Run("valid name update succeeds", func(t *testing.T) {
		h, _, adminTok := setup(t, "org-upd4")
		rr := h.do("PUT", "/api/v1/orgs/org-upd4", adminTok,
			jsonBody(UpdateOrgRequest{Name: strPtr("Renamed Org")}))
		assert.Equal(t, http.StatusOK, rr.Code)
		var dto OrgDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "Renamed Org", dto.Name)
	})
}

// ════════════════════════════════════════════════════════════════════════════
// PATCH /api/v1/admin/users/{id} — handlePatchUser (52.2%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandlePatchUserInvalidRole proves that an unknown system_role string
// returns 400, not 200 with garbage stored in the user row.
func TestHandlePatchUserInvalidRole(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	user, _ := h.mustCreateUser(t, "regular@example.com")

	invalidRole := "superuser"
	rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), adminTok,
		jsonBody(PatchUserRequest{SystemRole: &invalidRole}))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"an unrecognised system_role must be rejected with 400, not silently stored")
}

// TestHandlePatchUserNameChange verifies the UpdateUserFields path that
// handlePatchUser takes when only the name changes (no role change branch).
func TestHandlePatchUserNameChange(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	user, _ := h.mustCreateUser(t, "rename@example.com")

	newName := "Updated Name"
	rr := h.do("PATCH", "/api/v1/admin/users/"+strconv.FormatInt(user.ID, 10), adminTok,
		jsonBody(PatchUserRequest{Name: &newName}))
	require.Equal(t, http.StatusOK, rr.Code)

	var dto UserDTO
	decodeJSON(t, rr, &dto)
	assert.Equal(t, "Updated Name", dto.Name,
		"a name-only patch must be reflected immediately in the response")
}

// ════════════════════════════════════════════════════════════════════════════
// DELETE /api/v1/orgs/{id}/members/{email} — handleRemoveMember (56%)
// ════════════════════════════════════════════════════════════════════════════

func TestHandleRemoveMemberGaps(t *testing.T) {
	ctx := context.Background()

	t.Run("non-existent member returns 404", func(t *testing.T) {
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-rm1", Name: "Remove Member", Slug: "rm1", Status: org.StatusActive,
		})
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("DELETE", "/api/v1/orgs/org-rm1/members/nobody@example.com", adminTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"removing a non-existent member must return 404, not 204 or 500")
	})

	t.Run("non-owner caller cannot remove a sitting owner (403)", func(t *testing.T) {
		// An org-admin must not be able to remove an owner.
		// That would be a privilege escalation: after the remove, the admin is
		// the highest role in the org.
		h, orgStore, _ := newHarnessWithMT(t)
		ctx2 := context.Background()
		_ = orgStore.CreateOrg(ctx2, &org.Org{
			ID: "org-rm2", Name: "Remove Owner Test", Slug: "rm2", Status: org.StatusActive,
		})

		caller, callerTok := h.mustCreateUser(t, "rm-caller@example.com")
		_ = orgStore.AddOrgMember(ctx2, &org.Member{
			OrgID: "org-rm2", Email: caller.Email, Role: org.RoleAdmin,
		})
		// Sitting owner; also seed a second owner so last-owner guard does not
		// fire before the 403 ownership check.
		_ = orgStore.AddOrgMember(ctx2, &org.Member{
			OrgID: "org-rm2", Email: "owner-target@example.com", Role: org.RoleOwner,
		})
		_ = orgStore.AddOrgMember(ctx2, &org.Member{
			OrgID: "org-rm2", Email: "co-owner@example.com", Role: org.RoleOwner,
		})

		rr := h.do("DELETE", "/api/v1/orgs/org-rm2/members/owner-target@example.com",
			callerTok, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"an org-admin must not be permitted to remove a sitting owner")
	})

	t.Run("no org store returns 501", func(t *testing.T) {
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("DELETE", "/api/v1/orgs/org-rm/members/target@example.com", adminTok, nil)
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})
}

// ════════════════════════════════════════════════════════════════════════════
// GET /api/v1/admin/stats — handleStats (65.5%) store-fault paths
// ════════════════════════════════════════════════════════════════════════════

// TestHandleStatsStoreFaults proves that a stats-store failure produces 500,
// never 200 or a silently-empty body.
func TestHandleStatsStoreFaults(t *testing.T) {
	storeErr := errors.New("stats store unavailable")

	t.Run("ByProtocol error returns 500", func(t *testing.T) {
		h := newHarnessWithErrStats(t, &errStatsCollector{byProtoErr: storeErr})
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("GET", "/api/v1/admin/stats", adminTok, nil)
		assert.Equal(t, http.StatusInternalServerError, rr.Code,
			"a ByProtocol store error must answer 500, never 200")
	})

	t.Run("Total error returns 500", func(t *testing.T) {
		// ByProtocol succeeds; only Total fails.
		h := newHarnessWithErrStats(t, &errStatsCollector{totalErr: storeErr})
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("GET", "/api/v1/admin/stats", adminTok, nil)
		assert.Equal(t, http.StatusInternalServerError, rr.Code,
			"a Total store error must answer 500")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// GET /api/v1/admin/stats/series — handleStatsSeries (66.7%) store-fault paths
// ════════════════════════════════════════════════════════════════════════════

func TestHandleStatsSeriesStoreFaults(t *testing.T) {
	storeErr := errors.New("store failure")

	t.Run("Total error (grand-total path) returns 500", func(t *testing.T) {
		h := newHarnessWithErrStats(t, &errStatsCollector{totalErr: storeErr})
		_, adminTok := h.mustCreateAdmin(t)
		// No ?protocol= → grand-total path → calls Total.
		rr := h.do("GET", "/api/v1/admin/stats/series", adminTok, nil)
		assert.Equal(t, http.StatusInternalServerError, rr.Code,
			"a Total store error on the series grand-total path must answer 500")
	})

	t.Run("ByProtocol error (per-protocol path) returns 500", func(t *testing.T) {
		h := newHarnessWithErrStats(t, &errStatsCollector{byProtoErr: storeErr})
		_, adminTok := h.mustCreateAdmin(t)
		// ?protocol=oci → per-protocol path → calls ByProtocol.
		rr := h.do("GET", "/api/v1/admin/stats/series?protocol=oci", adminTok, nil)
		assert.Equal(t, http.StatusInternalServerError, rr.Code,
			"a ByProtocol store error on the per-protocol series path must answer 500")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// GET /api/v1/orgs — handleListOrgs (61.1%) API-key branch
// ════════════════════════════════════════════════════════════════════════════

// TestHandleListOrgsAPIKeyBranch exercises the path where the caller is an API
// key rather than a session user. The key must see ONLY its pinned org —
// cross-org listing via API key is denied by design.
func TestHandleListOrgsAPIKeyBranch(t *testing.T) {
	ctx := context.Background()
	h, orgStore, keyStore := newHarnessWithMT(t)

	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org-api-key1", Name: "API Key Org 1", Slug: "ak1", Status: org.StatusActive,
	})
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: "org-api-key2", Name: "API Key Org 2", Slug: "ak2", Status: org.StatusActive,
	})

	// API key pinned to org-api-key1 only.
	_, rawKey, err := keyStore.Create("org-api-key1", "list-orgs-key")
	require.NoError(t, err)

	rr := h.doWithBearer("GET", "/api/v1/orgs", rawKey, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp OrgsResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Orgs, 1,
		"an API key must only see its pinned org, not every org in the system")
	assert.Equal(t, "org-api-key1", resp.Orgs[0].ID)
}

// TestHandleListOrgsStoreError proves that a ListOrgsForEmail store failure
// returns 500, not 200 with an empty list.
func TestHandleListOrgsStoreError(t *testing.T) {
	h := newHarnessWithListOrgsErr(t)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/orgs", adminTok, nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code,
		"a ListOrgsForEmail store error must return 500, not 200 with an empty list")
}

// ════════════════════════════════════════════════════════════════════════════
// Key store: nil paths and List error (handleCreateKey 60%, handleListKeys 66%,
//            handleRevokeKey 63%)
// ════════════════════════════════════════════════════════════════════════════

// TestKeyStoreNilPaths proves that all key endpoints return 501 when
// Deps.KeyStore is nil.
func TestKeyStoreNilPaths(t *testing.T) {
	// The plain harness has no KeyStore.
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)

	t.Run("POST /api/v1/keys returns 501", func(t *testing.T) {
		rr := h.do("POST", "/api/v1/keys", adminTok, jsonBody(CreateKeyRequest{Label: "k"}))
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})

	t.Run("GET /api/v1/keys returns 501", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/keys", adminTok, nil)
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})

	t.Run("DELETE /api/v1/keys/{id} returns 501", func(t *testing.T) {
		rr := h.do("DELETE", "/api/v1/keys/kid_1", adminTok, nil)
		assert.Equal(t, http.StatusNotImplemented, rr.Code)
	})
}

// TestHandleCreateKeyBadBody proves that a malformed request body to the key
// creation endpoint returns 400, not 500.
func TestHandleCreateKeyBadBody(t *testing.T) {
	ctx := context.Background()
	h, orgStore, _ := newHarnessWithMT(t)
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: org.DefaultOrgID, Name: org.DefaultOrgName, Slug: org.DefaultOrgSlug,
		Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(ctx, &org.Member{
		OrgID: org.DefaultOrgID, Email: adminUser.Email, Role: org.RoleOwner,
	})

	rr := h.do("POST", "/api/v1/keys", adminTok, bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"a malformed key-creation body must return 400, not 500")
}

// TestHandleListKeysStoreError proves that a List failure on the key store
// returns 500, not 200 with an empty key list.
func TestHandleListKeysStoreError(t *testing.T) {
	ctx := context.Background()
	ek := &errKeyStore{
		fakeAPIKeyStore: newFakeAPIKeyStore(),
		listErr:         errors.New("db unavailable"),
	}
	h, orgStore := newHarnessWithErrKeys(t, ek)
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.CreateOrg(ctx, &org.Org{
		ID: org.DefaultOrgID, Name: org.DefaultOrgName, Slug: org.DefaultOrgSlug,
		Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(ctx, &org.Member{
		OrgID: org.DefaultOrgID, Email: adminUser.Email, Role: org.RoleOwner,
	})

	rr := h.do("GET", "/api/v1/keys", adminTok, nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code,
		"a key-store List error must return 500, not 200 with an empty list")
}

// ════════════════════════════════════════════════════════════════════════════
// canonicalProtocol: "go" → "gomod" alias (66.7%)
// ════════════════════════════════════════════════════════════════════════════

// TestCanonicalProtocolGoAlias proves that the "go" protocol alias is
// translated to "gomod" before lookup. Without this alias a
// GET /admin/cache/go would 404 on a "gomod" entry.
func TestCanonicalProtocolGoAlias(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)

	// The assertion is NOT 404: 404 would mean the alias was ignored and the
	// handler could not find the "gomod" protocol bucket.
	rr := h.do("GET", "/api/v1/admin/cache/go", adminTok, nil)
	assert.NotEqual(t, http.StatusNotFound, rr.Code,
		"GET /admin/cache/go must not 404; the 'go' alias must map to 'gomod'")
}

// ════════════════════════════════════════════════════════════════════════════
// PATCH /api/v1/invitations/{token} — handlePatchInvitation (60%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandlePatchInvitationInvalidStatus covers the default branch in the
// status switch — a status value that is neither "accepted" nor "declined".
func TestHandlePatchInvitationInvalidStatus(t *testing.T) {
	h, _, _ := newHarnessWithMT(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("PATCH", "/api/v1/invitations/some-token", tok,
		jsonBody(PatchInvitationRequest{Status: "revoked"}))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"an unrecognised invitation status must return 400, not be silently ignored")
}

// ════════════════════════════════════════════════════════════════════════════
// handlePatchUser: non-existent user → 404 (noop and non-noop paths) (55.2%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandlePatchUserUnknownUser proves that the noop (empty body) and
// non-noop (field set) branches of handlePatchUser both return 404 when the
// target user does not exist, rather than 500 or silently 200.
func TestHandlePatchUserUnknownUser(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	const ghostID = "99999"

	t.Run("noop body with unknown user returns 404", func(t *testing.T) {
		// req.Name==nil && req.SystemRole==nil && req.Password==nil → noop branch
		// → GetUserByID(99999) → ErrUserNotFound → 404
		rr := h.do("PATCH", "/api/v1/admin/users/"+ghostID, adminTok,
			jsonBody(PatchUserRequest{}))
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"a noop PATCH for a non-existent user must return 404, not 500 or 200")
	})

	t.Run("non-noop body with unknown user returns 404", func(t *testing.T) {
		// req.Name is set → non-noop path → GetUserByID(99999) → ErrUserNotFound
		newName := "Ghost User"
		rr := h.do("PATCH", "/api/v1/admin/users/"+ghostID, adminTok,
			jsonBody(PatchUserRequest{Name: &newName}))
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"a non-noop PATCH for a non-existent user must return 404, not 500 or 200")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// Cache: nil-meta 501 and unknown-protocol 404 for DELETE and PIN (47-60%)
// parseTier: consensus and tofu branches (66.7%)
// ════════════════════════════════════════════════════════════════════════════

// newHarnessNoMeta builds a server with Deps.Meta=nil so the nil-meta guard
// branches in all cache handlers can be exercised.
func newHarnessNoMeta(t *testing.T) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false, nil)
	srv := New(Deps{
		Stats:  newFakeStatsCollector(),
		Meta:   nil, // explicitly nil — exercises nil-meta guard
		Users:  store,
		Auth:   svc,
		Tokens: verifier,
		Config: testConfig(),
		Blobs:  &fakeBlobReporter{usedBytes: 1},
	})
	srv.hasher = hasher
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// TestDeleteCacheEntryNilMetaAndUnknownProtocol covers the two early-exit
// branches in handleDeleteCacheEntry that are not exercised by cache_test.go.
func TestDeleteCacheEntryNilMetaAndUnknownProtocol(t *testing.T) {
	t.Run("nil meta store returns 501", func(t *testing.T) {
		h := newHarnessNoMeta(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("DELETE", "/api/v1/admin/cache/oci/someentryid", adminTok, nil)
		assert.Equal(t, http.StatusNotImplemented, rr.Code,
			"DELETE /cache/{proto}/{id} must return 501 when the meta store is nil")
	})

	t.Run("unknown protocol returns 404", func(t *testing.T) {
		// Use a protocol that is not in knownProtocols (e.g. "ruby").
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("DELETE", "/api/v1/admin/cache/ruby/someentryid", adminTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"an unknown protocol on DELETE /cache must return 404, not 400 or 200")
	})
}

// TestPinCacheEntryNilMetaAndUnknownProtocol covers the two early-exit
// branches in handlePinCacheEntry that are not exercised by cache_test.go.
func TestPinCacheEntryNilMetaAndUnknownProtocol(t *testing.T) {
	t.Run("nil meta store returns 501", func(t *testing.T) {
		h := newHarnessNoMeta(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/admin/cache/oci/someentryid/pin", adminTok,
			jsonBody(PinCacheEntryRequest{Pinned: true}))
		assert.Equal(t, http.StatusNotImplemented, rr.Code,
			"POST /cache/{proto}/{id}/pin must return 501 when the meta store is nil")
	})

	t.Run("unknown protocol returns 404", func(t *testing.T) {
		h := newHarness(t)
		_, adminTok := h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/admin/cache/ruby/someentryid/pin", adminTok,
			jsonBody(PinCacheEntryRequest{Pinned: true}))
		assert.Equal(t, http.StatusNotFound, rr.Code,
			"an unknown protocol on POST /cache/pin must return 404, not 400 or 200")
	})
}

// TestParseTierConsensusAndTofu covers the consensus and tofu branches in
// parseTier which the existing cache_test.go filter tests do not exercise.
// These branches are deliberately chosen because an untested filter can silently
// return all rows when the value maps to the zero tier (which checksum is but
// consensus and tofu are not — so a missing branch means the filter is ignored).
func TestParseTierConsensusAndTofu(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)

	t.Run("?tier=consensus is a valid filter (200)", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/cache/oci?tier=consensus", adminTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code,
			"tier=consensus must be accepted; 400 means parseTier's consensus branch is unreachable")
	})

	t.Run("?tier=tofu is a valid filter (200)", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/admin/cache/oci?tier=tofu", adminTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code,
			"tier=tofu must be accepted; 400 means parseTier's tofu branch is unreachable")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// resolvePendingInvite: uncovered error paths (55.2%)
// ════════════════════════════════════════════════════════════════════════════

// TestResolvePendingInviteErrorPaths covers the branches in resolvePendingInvite
// that the existing org_ported_test.go and handleAcceptInvitation tests leave
// uncovered.
func TestResolvePendingInviteErrorPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("API key caller gets 403 (human login required)", func(t *testing.T) {
		// resolvePendingInvite checks !isUser and gates on human identity.
		// An API key has no email, so accepting on a human's behalf is forbidden.
		h, orgStore, keyStore := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-rpi1", Name: "RPI Org 1", Slug: "rpi1", Status: org.StatusActive,
		})
		_, rawKey, _ := keyStore.Create("org-rpi1", "rpi-key")

		rr := h.doWithBearer("PATCH", "/api/v1/invitations/some-token", rawKey,
			jsonBody(PatchInvitationRequest{Status: org.InviteStatusAccepted}))
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"an API key must be denied resolvePendingInvite with 403 (human login required)")
	})

	t.Run("empty token in accept body returns 400", func(t *testing.T) {
		// handleAcceptInvitation calls resolvePendingInvite(w, r, req.Token).
		// If req.Token is empty, the blank-token guard fires → 400.
		h, _, _ := newHarnessWithMT(t)
		_, tok := h.mustCreateAdmin(t)
		rr := h.do("POST", "/api/v1/invitations/accept", tok,
			jsonBody(AcceptInvitationRequest{Token: ""}))
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"an empty token in the accept body must return 400, not 404 or 500")
	})

	t.Run("non-pending invite (already accepted) returns 409", func(t *testing.T) {
		// An invite with Status="accepted" must not be re-accepted — the token
		// was already burned. Idempotent re-acceptance would let a leaked token
		// create duplicate memberships.
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-rpi2", Name: "RPI Org 2", Slug: "rpi2", Status: org.StatusActive,
		})
		invitee, inviteeTok := h.mustCreateUser(t, "rpi-invitee@example.com")

		inv := &org.Invitation{
			OrgID:     "org-rpi2",
			Email:     invitee.Email,
			Role:      org.RoleViewer,
			Token:     "already-accepted-token",
			Status:    org.InviteStatusAccepted, // already burned
			ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}
		_ = orgStore.CreateInvitation(ctx, inv)

		rr := h.do("POST", "/api/v1/invitations/accept", inviteeTok,
			jsonBody(AcceptInvitationRequest{Token: "already-accepted-token"}))
		assert.Equal(t, http.StatusConflict, rr.Code,
			"re-accepting an already-accepted invite must return 409, not 200")
	})

	t.Run("wrong-email invite returns 403", func(t *testing.T) {
		// The invitation is addressed to alice@example.com; bob accepts it.
		// Cross-account acceptance would give bob alice's intended membership.
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-rpi3", Name: "RPI Org 3", Slug: "rpi3", Status: org.StatusActive,
		})
		_, bobTok := h.mustCreateUser(t, "bob-rpi@example.com")

		inv := &org.Invitation{
			OrgID:     "org-rpi3",
			Email:     "alice-rpi@example.com", // invite addressed to alice
			Role:      org.RoleViewer,
			Token:     "alice-only-token",
			Status:    org.InviteStatusPending,
			ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}
		_ = orgStore.CreateInvitation(ctx, inv)

		// Bob (not alice) tries to accept alice's invitation.
		rr := h.do("POST", "/api/v1/invitations/accept", bobTok,
			jsonBody(AcceptInvitationRequest{Token: "alice-only-token"}))
		assert.Equal(t, http.StatusForbidden, rr.Code,
			"accepting an invitation addressed to a different email must return 403")
	})

	t.Run("expired invite returns 410", func(t *testing.T) {
		// An expired invitation must produce 410 (Gone), not 404 or 200.
		// The lazy-expiry convergence should also flip its status to "expired".
		h, orgStore, _ := newHarnessWithMT(t)
		_ = orgStore.CreateOrg(ctx, &org.Org{
			ID: "org-rpi4", Name: "RPI Org 4", Slug: "rpi4", Status: org.StatusActive,
		})
		invitee, inviteeTok := h.mustCreateUser(t, "rpi-expired@example.com")

		inv := &org.Invitation{
			OrgID:     "org-rpi4",
			Email:     invitee.Email,
			Role:      org.RoleViewer,
			Token:     "past-expiry-token",
			Status:    org.InviteStatusPending,
			ExpiresAt: time.Now().Add(-24 * time.Hour), // in the past
		}
		_ = orgStore.CreateInvitation(ctx, inv)

		rr := h.do("POST", "/api/v1/invitations/accept", inviteeTok,
			jsonBody(AcceptInvitationRequest{Token: "past-expiry-token"}))
		assert.Equal(t, http.StatusGone, rr.Code,
			"an expired invitation must return 410 Gone, triggering lazy-expiry convergence")
	})
}

// ════════════════════════════════════════════════════════════════════════════
// handleListInvitations: nil-orgs path (60%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleListInvitationsNilOrgs proves that the 501 guard fires when the
// org store is not configured.
func TestHandleListInvitationsNilOrgs(t *testing.T) {
	// Plain newHarness has no OrgStore.
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/orgs/org-any/invitations", adminTok, nil)
	assert.Equal(t, http.StatusNotImplemented, rr.Code,
		"GET /orgs/{id}/invitations must return 501 when the org store is not configured")
}

// ════════════════════════════════════════════════════════════════════════════
// handleConfig: nil-cfg 500 and sort-clause / nil-tiers branches (71.4%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleConfigNilCfg proves that GET /api/v1/admin/config returns 500
// when the server was initialised without a config.
func TestHandleConfigNilCfg(t *testing.T) {
	h := newHarnessWithCustomCfg(t, nil)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/admin/config", adminTok, nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code,
		"GET /admin/config with nil cfg must return 500, not panic or 200")
}

// TestHandleConfigSortAndNilTiers covers two branches in handleConfig that the
// base testConfig() does not reach:
//  1. The sort closure body — only reachable when ≥2 protocols are configured.
//  2. The nil-VerifyTiers normalisation — only reachable when Tiers is nil.
func TestHandleConfigSortAndNilTiers(t *testing.T) {
	cfg := testConfig()
	// Add a second protocol without Tiers so both branches are reachable.
	cfg.Protocols["pypi"] = config.ProtocolConfig{
		Upstreams: []config.UpstreamConfig{
			{Name: "pypi-upstream", BaseURL: "https://pypi.org", Priority: 0},
		},
		// Verification.Tiers is deliberately nil → exercises the nil-tiers branch.
	}

	h := newHarnessWithCustomCfg(t, cfg)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/admin/config", adminTok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp ConfigResponse
	decodeJSON(t, rr, &resp)
	// With 2 protocols and a sort, the returned list must be ordered.
	require.Len(t, resp.Protocols, 2, "both configured protocols must appear")
	assert.Equal(t, "oci", resp.Protocols[0].Protocol,
		"protocols must be sorted alphabetically; oci < pypi")
	// The pypi entry had nil Tiers — it must be normalised to [].
	var pypiView *ProtocolConfigView
	for i := range resp.Protocols {
		if resp.Protocols[i].Protocol == "pypi" {
			pypiView = &resp.Protocols[i]
			break
		}
	}
	require.NotNil(t, pypiView)
	assert.NotNil(t, pypiView.VerifyTiers,
		"a nil Tiers in the config must be normalised to an empty slice, not nil")
}

// ════════════════════════════════════════════════════════════════════════════
// handleCreateUser: malformed body → 400 (71.4%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleCreateUserBadBody proves that a malformed JSON body returns 400
// instead of panicking or returning 500.
func TestHandleCreateUserBadBody(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("POST", "/api/v1/admin/users", adminTok, bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"a malformed body must return 400, not panic or 500")
}

// ════════════════════════════════════════════════════════════════════════════
// handleGetUser: not-found → 404 (75%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleGetUserNotFound proves that a non-existent user ID returns 404.
func TestHandleGetUserNotFound(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/admin/users/99999", adminTok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code,
		"GET /admin/users/{id} for a non-existent user must return 404, not 500")
}

// ════════════════════════════════════════════════════════════════════════════
// handleLeaveOrg: nil org store → 501 (69.6%)
// ════════════════════════════════════════════════════════════════════════════

// TestHandleLeaveOrgNilStore proves that the 501 guard fires when the org
// store is not configured, preventing a nil-pointer panic.
func TestHandleLeaveOrgNilStore(t *testing.T) {
	h := newHarness(t)
	_, adminTok := h.mustCreateAdmin(t)
	rr := h.do("DELETE", "/api/v1/orgs/any-org/members/me", adminTok, nil)
	assert.Equal(t, http.StatusNotImplemented, rr.Code,
		"DELETE /orgs/{id}/members/me must return 501 when the org store is not configured")
}

// ════════════════════════════════════════════════════════════════════════════
// guardLastPrivileged: store error → 503 fail-CLOSED (68.4%)
// ════════════════════════════════════════════════════════════════════════════

// TestGuardLastPrivilegedStoreError proves the fail-CLOSED contract: when the
// count query fails, the privileged action is blocked with 503 rather than
// waved through. A guard that disables itself under DB trouble is exactly when
// the last owner gets removed.
func TestGuardLastPrivilegedStoreError(t *testing.T) {
	ctx := context.Background()
	storeErr := errors.New("count query failed")

	t.Run("CountOrgOwners error blocks owner demotion with 503", func(t *testing.T) {
		eos := &errOrgStore{
			fakeOrgStore:   newFakeOrgStore(),
			countOwnersErr: storeErr,
		}
		h, _ := newHarnessWithErrOrg(t, eos)

		_ = eos.CreateOrg(ctx, &org.Org{
			ID: "org-glp1", Name: "GLPOrg1", Slug: "glp1", Status: org.StatusActive,
		})
		// One owner — the ownership-count branch fires before the admin-count branch.
		_ = eos.AddOrgMember(ctx, &org.Member{
			OrgID: "org-glp1", Email: "owner@example.com", Role: org.RoleOwner,
		})

		_, adminTok := h.mustCreateAdmin(t)
		adminRole := org.RoleAdmin
		// Attempt to demote the owner → CountOrgOwners is called → returns error.
		rr := h.do("PATCH", "/api/v1/orgs/org-glp1/members/owner@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &adminRole}))
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
			"a CountOrgOwners failure must block the demotion with 503, never pass it through")
	})

	t.Run("CountOrgAdmins error blocks admin demotion with 503", func(t *testing.T) {
		eos := &errOrgStore{
			fakeOrgStore:   newFakeOrgStore(),
			countAdminsErr: storeErr,
		}
		h, _ := newHarnessWithErrOrg(t, eos)
		ctx2 := context.Background()

		_ = eos.CreateOrg(ctx2, &org.Org{
			ID: "org-glp2", Name: "GLPOrg2", Slug: "glp2", Status: org.StatusActive,
		})
		// One admin (not owner) — the admin-count branch fires, not the owner branch.
		_ = eos.AddOrgMember(ctx2, &org.Member{
			OrgID: "org-glp2", Email: "admin-mem@example.com", Role: org.RoleAdmin,
		})

		_, adminTok := h.mustCreateAdmin(t)
		viewerRole := org.RoleViewer
		// Attempt to demote the admin to viewer → CountOrgAdmins → error.
		rr := h.do("PATCH", "/api/v1/orgs/org-glp2/members/admin-mem@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &viewerRole}))
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
			"a CountOrgAdmins failure must block the demotion with 503, not silently pass through")
	})
}

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/store/meta"
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
			// oci is CAS-backed: its object count is exact.
			"oci": {Bytes: 2048, Objects: 3, ObjectsCountable: true},
		},
		total: artifact.SizeStat{Bytes: 2048, Objects: 3, ObjectsCountable: true},
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
func (c *fakeStatsCollector) AddOpaquePath(_, _ string)                              {}

// fakeBlobReporter implements BlobUsageReporter.
type fakeBlobReporter struct{ usedBytes int64 }

func (r *fakeBlobReporter) UsageBytes(_ context.Context) (int64, error) { return r.usedBytes, nil }

// fakeMetaStore is an in-memory meta.MetadataStore for the admin handler tests.
// ListEntries / SetPinned / Delete are backed by a real slice so the cache
// browser endpoints can be exercised end-to-end (filtering, sorting, paging);
// the remaining methods are inert stubs the admin API never calls.
type fakeMetaStore struct {
	mu      sync.Mutex
	entries []meta.Entry
	listErr error
}

// seed replaces the store's contents. Each entry's ID is derived exactly as a
// real driver would, so tests address rows through the same opaque IDs the API
// hands to clients.
func (m *fakeMetaStore) seed(entries ...meta.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = nil
	for _, e := range entries {
		e.Protocol = e.Ref.Protocol
		e.ID = meta.EncodeEntryID(e.Ref)
		m.entries = append(m.entries, e)
	}
}

func (m *fakeMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (m *fakeMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error { return nil }

func (m *fakeMetaStore) Delete(_ context.Context, ref artifact.ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.Ref.Protocol == ref.Protocol && e.Ref.Name == ref.Name && e.Ref.Version == ref.Version {
			continue
		}
		kept = append(kept, e)
	}
	m.entries = kept
	return nil
}

func (m *fakeMetaStore) GetMutable(_ context.Context, _ string) (*artifact.MutableEntry, error) {
	return nil, nil
}
func (m *fakeMetaStore) PutMutable(_ context.Context, _ artifact.MutableEntry) error { return nil }
func (m *fakeMetaStore) DeleteMutable(_ context.Context, _ string) error             { return nil }
func (m *fakeMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

// ListEntries mirrors the drivers' contract: filter, then order, then window,
// reporting Total against the filter (not the window).
func (m *fakeMetaStore) ListEntries(_ context.Context, protocol string, f meta.EntryFilter, p meta.Page) (meta.EntryPage, error) {
	if m.listErr != nil {
		return meta.EntryPage{}, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p = p.Normalize()

	var matched []meta.Entry
	for _, e := range m.entries {
		if protocol != "" && e.Ref.Protocol != protocol {
			continue
		}
		if f.NameContains != "" && !strings.Contains(e.Ref.Name, f.NameContains) {
			continue
		}
		if f.Tier != nil && e.Tier != *f.Tier {
			continue
		}
		if f.Upstream != "" && e.Upstream != f.Upstream {
			continue
		}
		if f.Pinned != nil && e.Pinned != *f.Pinned {
			continue
		}
		matched = append(matched, e)
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		var less bool
		switch p.Sort {
		case meta.SortSize:
			less = a.Size < b.Size
		case meta.SortName:
			less = a.Ref.Name < b.Ref.Name
		case meta.SortVerifiedAt:
			less = a.VerifiedAt.Before(b.VerifiedAt)
		default:
			less = a.CreatedAt.Before(b.CreatedAt)
		}
		if p.Desc {
			return !less
		}
		return less
	})

	total := int64(len(matched))
	start := p.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + p.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return meta.EntryPage{
		Entries: matched[start:end],
		Total:   total,
		Limit:   p.Limit,
		Offset:  p.Offset,
	}, nil
}

func (m *fakeMetaStore) SetPinned(_ context.Context, ref artifact.ArtifactRef, pinned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.entries {
		e := &m.entries[i]
		if e.Ref.Protocol == ref.Protocol && e.Ref.Name == ref.Name && e.Ref.Version == ref.Version {
			e.Pinned = pinned
		}
	}
	return nil
}

// ---- fake org store ----------------------------------------------------------

// fakeOrgStore is a thread-safe in-memory org.Store + auth.OrgResolver for tests.
type fakeOrgStore struct {
	mu          sync.Mutex
	orgs        map[string]*org.Org
	members     map[string]map[string]*org.Member // orgID → lowerEmail → member
	invitations map[string]*org.Invitation        // id → invitation
	byToken     map[string]*org.Invitation        // token → invitation
	nextMembID  int
	nextInvID   int
}

func newFakeOrgStore() *fakeOrgStore {
	return &fakeOrgStore{
		orgs:        map[string]*org.Org{},
		members:     map[string]map[string]*org.Member{},
		invitations: map[string]*org.Invitation{},
		byToken:     map[string]*org.Invitation{},
	}
}

func (f *fakeOrgStore) CreateOrg(_ context.Context, o *org.Org) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o.ID == "" {
		o.ID = "org_" + strconv.Itoa(len(f.orgs)+1)
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	cp := *o
	f.orgs[o.ID] = &cp
	return nil
}

func (f *fakeOrgStore) GetOrg(_ context.Context, id string) (*org.Org, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orgs[id]
	if !ok {
		return nil, org.ErrNotFound
	}
	cp := *o
	return &cp, nil
}

func (f *fakeOrgStore) GetOrgBySlug(_ context.Context, slug string) (*org.Org, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orgs {
		if o.Slug == slug {
			cp := *o
			return &cp, nil
		}
	}
	return nil, org.ErrNotFound
}

func (f *fakeOrgStore) ListOrgs(_ context.Context) ([]*org.Org, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*org.Org, 0, len(f.orgs))
	for _, o := range f.orgs {
		cp := *o
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeOrgStore) UpdateOrg(_ context.Context, o *org.Org) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orgs[o.ID]; !ok {
		return org.ErrNotFound
	}
	cp := *o
	f.orgs[o.ID] = &cp
	return nil
}

func (f *fakeOrgStore) DeleteOrg(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orgs[id]; !ok {
		return org.ErrNotFound
	}
	delete(f.orgs, id)
	delete(f.members, id)
	return nil
}

func (f *fakeOrgStore) ListOrgsForEmail(_ context.Context, email string) ([]*org.Org, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	email = strings.ToLower(email)
	var out []*org.Org
	for orgID, memberMap := range f.members {
		if m, ok := memberMap[email]; ok {
			o, exists := f.orgs[orgID]
			if exists {
				cp := *o
				cp.Role = m.Role
				out = append(out, &cp)
			}
		}
	}
	return out, nil
}

func (f *fakeOrgStore) SetOrgStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orgs[id]
	if !ok {
		return org.ErrNotFound
	}
	o.Status = status
	return nil
}

func (f *fakeOrgStore) CountOrgAdmins(_ context.Context, orgID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, m := range f.members[orgID] {
		if org.AtLeast(m.Role, org.RoleAdmin) {
			count++
		}
	}
	return count, nil
}

func (f *fakeOrgStore) CountOrgOwners(_ context.Context, orgID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, m := range f.members[orgID] {
		if m.Role == org.RoleOwner {
			count++
		}
	}
	return count, nil
}

func (f *fakeOrgStore) CountOrgs(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.orgs), nil
}

func (f *fakeOrgStore) CountOrgsByCreator(_ context.Context, userID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.orgs {
		if o.CreatedBy == userID {
			n++
		}
	}
	return n, nil
}

func (f *fakeOrgStore) AddOrgMember(_ context.Context, m *org.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	email := strings.ToLower(m.Email)
	if f.members[m.OrgID] == nil {
		f.members[m.OrgID] = map[string]*org.Member{}
	}
	existing, exists := f.members[m.OrgID][email]
	if exists {
		// Upsert: update role.
		existing.Role = m.Role
		if m.InvitedBy != "" {
			existing.InvitedBy = m.InvitedBy
		}
		return nil
	}
	f.nextMembID++
	cp := *m
	cp.Email = email
	if cp.ID == "" {
		cp.ID = "mem_" + strconv.Itoa(f.nextMembID)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	f.members[m.OrgID][email] = &cp
	return nil
}

func (f *fakeOrgStore) GetOrgMember(_ context.Context, orgID, email string) (*org.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	email = strings.ToLower(email)
	m, ok := f.members[orgID][email]
	if !ok {
		return nil, org.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (f *fakeOrgStore) ListOrgMembers(_ context.Context, orgID string) ([]*org.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*org.Member, 0, len(f.members[orgID]))
	for _, m := range f.members[orgID] {
		cp := *m
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeOrgStore) RemoveOrgMember(_ context.Context, orgID, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	email = strings.ToLower(email)
	if _, ok := f.members[orgID][email]; !ok {
		return org.ErrNotFound
	}
	delete(f.members[orgID], email)
	return nil
}

func (f *fakeOrgStore) CreateInvitation(_ context.Context, inv *org.Invitation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextInvID++
	if inv.ID == "" {
		inv.ID = "inv_" + strconv.Itoa(f.nextInvID)
	}
	if inv.Token == "" {
		inv.Token = "tok_" + strconv.Itoa(f.nextInvID)
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}
	cp := *inv
	f.invitations[inv.ID] = &cp
	f.byToken[inv.Token] = &cp
	return nil
}

func (f *fakeOrgStore) GetInvitationByToken(_ context.Context, token string) (*org.Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.byToken[token]
	if !ok {
		return nil, org.ErrNotFound
	}
	cp := *inv
	return &cp, nil
}

func (f *fakeOrgStore) ListInvitations(_ context.Context, orgID string) ([]*org.Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*org.Invitation
	for _, inv := range f.invitations {
		if inv.OrgID == orgID {
			cp := *inv
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeOrgStore) SetInvitationStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.invitations[id]
	if !ok {
		return org.ErrNotFound
	}
	inv.Status = status
	if tk, ok2 := f.byToken[inv.Token]; ok2 {
		tk.Status = status
	}
	return nil
}

// ---- fake apikey store -------------------------------------------------------

// fakeAPIKeyStore is a fully functional in-memory apikey.Store for tests.
// Unlike the production MemStore (stub), this implementation is complete.
type fakeAPIKeyStore struct {
	mu      sync.Mutex
	byID    map[string]*apikey.KeyInfo // id → info (includes OrgID)
	rawKeys map[string]string          // id → plaintext key (for LookupSubject)
}

func newFakeAPIKeyStore() *fakeAPIKeyStore {
	return &fakeAPIKeyStore{
		byID:    map[string]*apikey.KeyInfo{},
		rawKeys: map[string]string{},
	}
}

func (f *fakeAPIKeyStore) create(orgID, userID, label string) (string, string, error) {
	// Use public helpers via a raw key construction.
	rawKey := apikey.KeyPrefix + "testkey" + strconv.Itoa(len(f.byID)+1)
	id := "kid_" + strconv.Itoa(len(f.byID)+1)
	info := &apikey.KeyInfo{
		ID:        id,
		OrgID:     orgID,
		UserID:    userID,
		Label:     label,
		Prefix:    rawKey[:len(apikey.KeyPrefix)+6] + "…",
		CreatedAt: time.Now().UTC(),
		Revoked:   false,
	}
	f.byID[id] = info
	f.rawKeys[id] = rawKey
	return id, rawKey, nil
}

func (f *fakeAPIKeyStore) Create(orgID, label string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if orgID == "" {
		orgID = apikey.DefaultOrgID
	}
	return f.create(orgID, "", label)
}

func (f *fakeAPIKeyStore) CreateOwned(orgID, userID, label string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if orgID == "" {
		orgID = apikey.DefaultOrgID
	}
	return f.create(orgID, userID, label)
}

func (f *fakeAPIKeyStore) LookupSubject(token string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, raw := range f.rawKeys {
		if raw == token {
			info, ok := f.byID[id]
			if !ok || info.Revoked {
				return "", "", false
			}
			if info.ExpiresAt != nil && time.Now().After(*info.ExpiresAt) {
				return "", "", false
			}
			return info.OrgID, apikey.SubjectID(id), true
		}
	}
	return "", "", false
}

func (f *fakeAPIKeyStore) List(orgID string) ([]apikey.KeyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apikey.KeyInfo
	for _, info := range f.byID {
		if info.OrgID == orgID {
			out = append(out, *info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeAPIKeyStore) Get(orgID, id string) (apikey.KeyInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.byID[id]
	if !ok || info.OrgID != orgID {
		return apikey.KeyInfo{}, false
	}
	return *info, true
}

func (f *fakeAPIKeyStore) Revoke(orgID, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.byID[id]
	if !ok || info.OrgID != orgID {
		return false, nil
	}
	if info.Revoked {
		return false, nil
	}
	info.Revoked = true
	return true, nil
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
	stats    *fakeStatsCollector // preset stats data; mutate before calling /admin/stats
}

// newHarness creates a complete admin server wired with fakes (no multi-tenant stores).
func newHarness(t *testing.T) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false, nil)

	stats := newFakeStatsCollector()
	srv := New(Deps{
		Stats:  stats,
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

	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher, stats: stats}
}

// newHarnessWithMT creates a harness with multi-tenant (org + apikey + grant) stores.
func newHarnessWithMT(t *testing.T) (*harness, *fakeOrgStore, *fakeAPIKeyStore) {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	orgStore := newFakeOrgStore()
	keyStore := newFakeAPIKeyStore()

	svc := auth.NewService(store, hasher, verifier, false, nil) // bootstrap not tested here

	srv := New(Deps{
		Stats:    newFakeStatsCollector(),
		Meta:     &fakeMetaStore{},
		Users:    store,
		Auth:     svc,
		Tokens:   verifier,
		Config:   testConfig(),
		Blobs:    &fakeBlobReporter{usedBytes: 999},
		Secure:   false,
		Logger:   nil,
		OrgStore: orgStore,
		KeyStore: keyStore,
	})
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	h := &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
	return h, orgStore, keyStore
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

// Without an upstream Registry wired in, the endpoint must still enumerate the
// configured chain — but flag it as not live, so the UI renders "—" for every
// measurement rather than presenting config as observed fact.
func TestHandleUpstreams_ConfigOnlyWhenNoRuntime(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/upstreams", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp UpstreamsResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Protocols, 1)

	p := resp.Protocols[0]
	assert.Equal(t, "oci", p.Protocol)
	assert.False(t, p.Live, "no Registry wired in ⇒ not live")
	require.Len(t, p.Mirrors, 1)
	assert.Equal(t, "dockerhub", p.Mirrors[0].Name)
	assert.Equal(t, "https://registry-1.docker.io", p.Mirrors[0].URL)
	assert.Equal(t, "unknown", p.Mirrors[0].Health, "unmeasured must not read as up")
	assert.False(t, p.Mirrors[0].Blocked)
	assert.False(t, p.Mirrors[0].HasLatency)
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

// ======================================================================
// Multi-tenant tests (apikey resolution, X-Org-Id, keys CRUD, members, /me)
// ======================================================================

// doWithBearer makes a request using Authorization: Bearer <token>.
func (h *harness) doWithBearer(method, path, bearer string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.mux.ServeHTTP(rr, req)
	return rr
}

// doWithOrgHeader makes a cookie-authenticated request with an X-Org-Id header.
func (h *harness) doWithOrgHeader(method, path, token, orgID string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.TokenCookieName, Value: token})
	}
	if orgID != "" {
		req.Header.Set("X-Org-Id", orgID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.mux.ServeHTTP(rr, req)
	return rr
}

// ---- apikey → subject resolution --------------------------------------------

func TestAPIKeySubjectResolution(t *testing.T) {
	h, _, keyStore := newHarnessWithMT(t)

	t.Run("valid apikey resolves to subject", func(t *testing.T) {
		// Create a key directly in the fake store.
		id, rawKey, err := keyStore.CreateOwned(org.DefaultOrgID, "user:1", "test-key")
		require.NoError(t, err)
		require.NotEmpty(t, rawKey)

		// POST /api/v1/keys (list) with the apikey as Bearer.
		rr := h.doWithBearer("GET", "/api/v1/keys", rawKey, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "valid apikey must pass PrincipalMiddleware")

		var resp KeysResponse
		decodeJSON(t, rr, &resp)
		// The key we just created must appear in the list.
		found := false
		for _, k := range resp.Keys {
			if k.ID == id {
				found = true
			}
		}
		assert.True(t, found, "created key must appear in list response")
	})

	t.Run("unknown apikey returns 401", func(t *testing.T) {
		rr := h.doWithBearer("GET", "/api/v1/keys", "spck_notarealkey123456", nil)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("no credentials returns 401 for authed routes", func(t *testing.T) {
		rr := h.doWithBearer("GET", "/api/v1/keys", "", nil)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

// ---- X-Org-Id header role resolution ----------------------------------------

func TestXOrgIdRole(t *testing.T) {
	h, orgStore, _ := newHarnessWithMT(t)

	// Seed an org and a member.
	_ = orgStore.CreateOrg(context.Background(), &org.Org{
		ID: "org_test", Name: "Test", Slug: "test", Status: org.StatusActive,
	})
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.AddOrgMember(context.Background(), &org.Member{
		OrgID: "org_test", Email: adminUser.Email, Role: org.RoleOwner,
	})

	t.Run("session user with X-Org-Id gets org context in /me", func(t *testing.T) {
		rr := h.doWithOrgHeader("GET", "/api/v1/me", adminTok, "org_test", nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, "org_test", resp.ActiveOrgID)
		assert.Equal(t, org.RoleOwner, resp.ActiveOrgRole)
	})

	t.Run("non-member with explicit X-Org-Id gets 403", func(t *testing.T) {
		_, nonMemberTok := h.mustCreateUser(t, "nonmember@example.com")
		// nonmember is not in org_test, so PrincipalMiddleware should 403
		// when they explicitly request that org.
		rr := h.doWithOrgHeader("GET", "/api/v1/keys", nonMemberTok, "org_test", nil)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("system admin can cross-org with X-Org-Id", func(t *testing.T) {
		// admin is system_role=admin, so cross-org is allowed.
		// (admin is already a member of org_test so this isn't a true cross-org
		// test, but it verifies the path doesn't block admins.)
		rr := h.doWithOrgHeader("GET", "/api/v1/keys", adminTok, "org_test", nil)
		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

// ---- keys CRUD ---------------------------------------------------------------

func TestKeysCRUD(t *testing.T) {
	h, orgStore, _ := newHarnessWithMT(t)

	// Bootstrap: create default org and add admin as owner.
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.CreateOrg(context.Background(), &org.Org{
		ID: org.DefaultOrgID, Name: org.DefaultOrgName, Slug: org.DefaultOrgSlug,
		Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(context.Background(), &org.Member{
		OrgID: org.DefaultOrgID, Email: adminUser.Email, Role: org.RoleOwner,
	})

	t.Run("create key returns plaintext once", func(t *testing.T) {
		rr := h.do("POST", "/api/v1/keys", adminTok, jsonBody(CreateKeyRequest{Label: "ci-key"}))
		assert.Equal(t, http.StatusCreated, rr.Code)

		var dto KeyDTO
		decodeJSON(t, rr, &dto)
		assert.NotEmpty(t, dto.ID)
		assert.NotEmpty(t, dto.RawKey, "raw key must be returned on creation")
		assert.True(t, strings.HasPrefix(dto.RawKey, apikey.KeyPrefix), "key must start with spck_")
		assert.Equal(t, "ci-key", dto.Label)
		assert.False(t, dto.Revoked)
	})

	t.Run("list keys returns created key", func(t *testing.T) {
		// Create a second key.
		rr := h.do("POST", "/api/v1/keys", adminTok, jsonBody(CreateKeyRequest{Label: "deploy-key"}))
		require.Equal(t, http.StatusCreated, rr.Code)

		listRR := h.do("GET", "/api/v1/keys", adminTok, nil)
		assert.Equal(t, http.StatusOK, listRR.Code)

		var resp KeysResponse
		decodeJSON(t, listRR, &resp)
		assert.NotEmpty(t, resp.Keys)
		// RawKey must NOT appear in list responses.
		for _, k := range resp.Keys {
			assert.Empty(t, k.RawKey, "raw key must not appear in list response")
		}
	})

	t.Run("revoke key returns 204", func(t *testing.T) {
		createRR := h.do("POST", "/api/v1/keys", adminTok, jsonBody(CreateKeyRequest{Label: "temp-key"}))
		require.Equal(t, http.StatusCreated, createRR.Code)

		var created KeyDTO
		decodeJSON(t, createRR, &created)

		revokeRR := h.do("DELETE", "/api/v1/keys/"+created.ID, adminTok, nil)
		assert.Equal(t, http.StatusNoContent, revokeRR.Code)

		// After revocation, the raw key must no longer authenticate.
		authRR := h.doWithBearer("GET", "/api/v1/keys", created.RawKey, nil)
		assert.Equal(t, http.StatusUnauthorized, authRR.Code, "revoked key must be rejected")
	})

	t.Run("revoke unknown key returns 404", func(t *testing.T) {
		rr := h.do("DELETE", "/api/v1/keys/doesnotexist", adminTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("apikey can create and list keys in its own org", func(t *testing.T) {
		// Create a key via session.
		createRR := h.do("POST", "/api/v1/keys", adminTok, jsonBody(CreateKeyRequest{Label: "apikey-test"}))
		require.Equal(t, http.StatusCreated, createRR.Code)
		var apikeyDTO KeyDTO
		decodeJSON(t, createRR, &apikeyDTO)

		// Use that key to list keys (apikey authenticates via PrincipalMiddleware).
		listRR := h.doWithBearer("GET", "/api/v1/keys", apikeyDTO.RawKey, nil)
		assert.Equal(t, http.StatusOK, listRR.Code)
	})
}

// ---- member management -------------------------------------------------------

func TestMemberManagement(t *testing.T) {
	h, orgStore, _ := newHarnessWithMT(t)

	// Bootstrap org.
	adminUser, adminTok := h.mustCreateAdmin(t)
	_ = orgStore.CreateOrg(context.Background(), &org.Org{
		ID: "org_main", Name: "Main", Slug: "main", Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(context.Background(), &org.Member{
		OrgID: "org_main", Email: adminUser.Email, Role: org.RoleOwner,
	})

	t.Run("list members returns owner", func(t *testing.T) {
		rr := h.do("GET", "/api/v1/orgs/org_main/members", adminTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MembersResponse
		decodeJSON(t, rr, &resp)
		require.Len(t, resp.Members, 1)
		assert.Equal(t, adminUser.Email, resp.Members[0].Email)
		assert.Equal(t, org.RoleOwner, resp.Members[0].Role)
	})

	t.Run("add member succeeds", func(t *testing.T) {
		rr := h.do("POST", "/api/v1/orgs/org_main/members", adminTok,
			jsonBody(AddMemberRequest{Email: "newmember@example.com", Role: org.RoleEditor}))
		assert.Equal(t, http.StatusCreated, rr.Code)

		var dto MemberDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, "newmember@example.com", dto.Email)
		assert.Equal(t, org.RoleEditor, dto.Role)
	})

	t.Run("patch member role", func(t *testing.T) {
		// Add another member first.
		h.do("POST", "/api/v1/orgs/org_main/members", adminTok,
			jsonBody(AddMemberRequest{Email: "patchme@example.com", Role: org.RoleViewer}))

		newRole := org.RoleAdmin
		rr := h.do("PATCH", "/api/v1/orgs/org_main/members/patchme@example.com", adminTok,
			jsonBody(PatchMemberRequest{Role: &newRole}))
		assert.Equal(t, http.StatusOK, rr.Code)

		var dto MemberDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, org.RoleAdmin, dto.Role)
	})

	t.Run("remove member succeeds", func(t *testing.T) {
		h.do("POST", "/api/v1/orgs/org_main/members", adminTok,
			jsonBody(AddMemberRequest{Email: "todelete@example.com", Role: org.RoleViewer}))

		rr := h.do("DELETE", "/api/v1/orgs/org_main/members/todelete@example.com", adminTok, nil)
		assert.Equal(t, http.StatusNoContent, rr.Code)

		_, err := orgStore.GetOrgMember(context.Background(), "org_main", "todelete@example.com")
		assert.ErrorIs(t, err, org.ErrNotFound)
	})

	t.Run("non-admin cannot manage members", func(t *testing.T) {
		_, userTok := h.mustCreateUser(t, "plain@example.com")
		// plain is not a member of org_main, so requireOrgAdmin returns 403.
		rr := h.do("GET", "/api/v1/orgs/org_main/members", userTok, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("member viewer cannot manage members", func(t *testing.T) {
		// Add a viewer to the org.
		h.do("POST", "/api/v1/orgs/org_main/members", adminTok,
			jsonBody(AddMemberRequest{Email: "viewer@example.com", Role: org.RoleViewer}))

		_, viewerTok := h.mustCreateUser(t, "viewer@example.com")
		rr := h.do("GET", "/api/v1/orgs/org_main/members", viewerTok, nil)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})
}

// ---- /me org context ---------------------------------------------------------

func TestMeOrgContext(t *testing.T) {
	h, orgStore, _ := newHarnessWithMT(t)

	adminUser, adminTok := h.mustCreateAdmin(t)

	// Seed an org with the admin as owner.
	_ = orgStore.CreateOrg(context.Background(), &org.Org{
		ID: org.DefaultOrgID, Name: org.DefaultOrgName, Slug: org.DefaultOrgSlug,
		Status: org.StatusActive,
	})
	_ = orgStore.AddOrgMember(context.Background(), &org.Member{
		OrgID: org.DefaultOrgID, Email: adminUser.Email, Role: org.RoleOwner,
	})

	t.Run("/me lists memberships; no X-Org-Id means no active org", func(t *testing.T) {
		// Without a header the caller has named no org, so there is no active
		// one to report. Defaulting to org_default here was the phantom
		// membership: it claimed an org the caller had not selected, and for a
		// non-member claimed one they had no right to at all.
		rr := h.do("GET", "/api/v1/me", adminTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, adminUser.ID, resp.User.ID)
		assert.True(t, resp.IsAdmin, "first user must be admin")
		assert.Empty(t, resp.ActiveOrgID, "no X-Org-Id means no active org")
		assert.Empty(t, resp.ActiveOrgRole)
		// The membership list is still the truth, and is what the client uses
		// to pick an org to send back as X-Org-Id.
		require.Len(t, resp.Orgs, 1)
		assert.Equal(t, org.DefaultOrgID, resp.Orgs[0].ID)
	})

	t.Run("/me with X-Org-Id reports the resolved org and role", func(t *testing.T) {
		rr := h.doWithOrgHeader("GET", "/api/v1/me", adminTok, org.DefaultOrgID, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, org.DefaultOrgID, resp.ActiveOrgID)
		assert.Equal(t, org.RoleOwner, resp.ActiveOrgRole)
		assert.False(t, resp.ActiveOrgSystemAccess, "a real member is not system access")
	})

	t.Run("/me without org store returns just user", func(t *testing.T) {
		// Use a plain harness (no org store).
		plainH := newHarness(t)
		plainAdmin, plainTok := plainH.mustCreateAdmin(t)

		rr := plainH.do("GET", "/api/v1/me", plainTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.Equal(t, plainAdmin.ID, resp.User.ID)
		assert.Empty(t, resp.ActiveOrgID, "no org store means no org context")
	})

	t.Run("/me is_admin field", func(t *testing.T) {
		_, userTok := h.mustCreateUser(t, "regular@example.com")
		rr := h.do("GET", "/api/v1/me", userTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp MeResponse
		decodeJSON(t, rr, &resp)
		assert.False(t, resp.IsAdmin, "non-admin user must have is_admin=false")
	})
}

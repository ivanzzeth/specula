//go:build integration

// Package e2e — hermetic end-to-end tests for the WRITABLE, multi-tenant hosted
// registry (REGISTRY-DESIGN R2). Everything runs in-process: a real Specula
// data-plane stack (local CAS + sqlite + org/apikey/repo stores + RS256 token
// service + acl authz glue) is served from an httptest.Server, and
// go-containerregistry's remote client drives the full Docker v2 Bearer token
// dance (docker login → /token → push → pull) with Basic(email:apikey) auth.
//
// Coverage:
//   - push an image into <org>/<repo>:tag with an org API key, pull it back,
//     assert the round-tripped manifest digest + config bytes match (the token
//     dance, chunked blob push, hosted CAS write, and hosted-first pull are all
//     exercised);
//   - (a) an unauthenticated pull of a private repo → 401;
//   - (b) after flipping the repo to public, an ANONYMOUS crane pull succeeds;
//   - (c) a push with a DIFFERENT org's API key is denied (cross-org isolation).
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ggcrauthn "github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/apikey"
	"github.com/ivanzzeth/specula/internal/cache"
	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
	registryhandler "github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/registryauthz"
	"github.com/ivanzzeth/specula/internal/registrytoken"
	"github.com/ivanzzeth/specula/internal/repo"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// ────────────────────────────────────────────────────────────────────────────
// registryStack — the full writable-registry data-plane stack
// ────────────────────────────────────────────────────────────────────────────

type registryStack struct {
	srv   *httptest.Server
	host  string // bare host:port
	repos *repo.SQLStore
	orgs  org.Store
	keys  apikey.Store
}

// seededOrg holds an org's identity plus a freshly minted API key (plaintext).
type seededOrg struct {
	id     string
	slug   string
	rawKey string
}

// newRegistryStack builds and starts the writable-registry stack, seeding each
// requested org slug with an org row and an org-scoped API key. The returned
// stack's host serves /v2/ (Challenge → registry-write → oci-read) and /token.
func newRegistryStack(t *testing.T, orgSlugs ...string) (*registryStack, map[string]seededOrg) {
	return newRegistryStackWithOpts(t, nil, orgSlugs...)
}

// newRegistryStackWithOpts is newRegistryStack with extra OCI handler options
// (e.g. an upstream for exercising pull-through alongside the writable registry).
func newRegistryStackWithOpts(t *testing.T, extraOCI []ocihandler.Option, orgSlugs ...string) (*registryStack, map[string]seededOrg) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	bs := local.NewLocalDiskDriver(blobDir)

	ms, err := sqlite.NewSQLiteStore(filepath.Join(dir, "specula.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ms.Close() })

	chain := verify.NewChain(verify.NewChecksumVerifier(), verify.NewTofuVerifier(newInMemTofuStore()))
	cm := cache.New(bs, ms, chain)

	db := ms.DB()
	orgs := org.NewSQLStore(db)
	keys := apikey.NewSQLStore(db)
	repos := repo.NewSQLStore(db)

	seeded := make(map[string]seededOrg, len(orgSlugs))
	for i, slug := range orgSlugs {
		orgID := fmt.Sprintf("org_%d", i+1)
		require.NoError(t, orgs.CreateOrg(ctx, &org.Org{
			ID: orgID, Name: slug, Slug: slug, Status: org.StatusActive, CreatedBy: "user:seed",
		}))
		keyID, rawKey, err := keys.CreateOwned(orgID, "user:seed", "e2e-"+slug)
		require.NoError(t, err)
		require.NotEmpty(t, keyID)
		require.True(t, strings.HasPrefix(rawKey, apikey.KeyPrefix))
		seeded[slug] = seededOrg{id: orgID, slug: slug, rawKey: rawKey}
	}

	// RS256 token service + acl authz glue.
	key, err := registrytoken.EnsureKeyPair(filepath.Join(dir, "reg-token.pem"))
	require.NoError(t, err)
	tokenSvc := registrytoken.NewService(key, "specula", "specula", 0)
	authz := registryauthz.New(orgs, repos)

	// /v2/ chain: Challenge → registry(write) → oci(read, hosted-first).
	ociOpts := append([]ocihandler.Option{
		ocihandler.WithMeta(ms),
		ocihandler.WithHostedResolver(authz),
		ocihandler.WithHostedReadAuthz(authz),
		ocihandler.WithOwnedNamespaceResolver(authz),
	}, extraOCI...)
	ociRead := ocihandler.NewHandler(cm, ociOpts...)
	writeHandler := registryhandler.NewHandler(cm, repos, repos, authz,
		registryhandler.WithBlobStore(bs),
		registryhandler.WithMeta(ms),
		registryhandler.WithReadHandler(ociRead),
	)
	realmFor := func(r *http.Request) string { return "http://" + r.Host + "/token" }
	challenge := tokenSvc.ChallengeFunc(realmFor)

	authn := &registrytoken.BasicAuthenticator{Keys: keys}
	tokenHandler := registrytoken.NewTokenHandler(tokenSvc, authn, authz)

	mux := http.NewServeMux()
	mux.Handle("/v2/", challenge(writeHandler))
	mux.Handle("/token", tokenHandler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &registryStack{
		srv:   srv,
		host:  strings.TrimPrefix(srv.URL, "http://"),
		repos: repos,
		orgs:  orgs,
		keys:  keys,
	}, seeded
}

// ref builds an insecure name.Reference for host/repo:tag.
func (s *registryStack) ref(t *testing.T, repoName, tag string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", s.host, repoName, tag), name.Insecure)
	require.NoError(t, err)
	return r
}

// basicAuth returns a go-containerregistry authenticator for email:apikey.
func basicAuth(email, key string) remote.Option {
	return remote.WithAuth(&ggcrauthn.Basic{Username: email, Password: key})
}

// ────────────────────────────────────────────────────────────────────────────
// Test — full push → pull round trip through the Bearer token dance
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryPushPullRoundTrip(t *testing.T) {
	stack, seeded := newRegistryStack(t, "org1")
	key := seeded["org1"].rawKey
	auth := basicAuth("ci@example.com", key)

	// Build a tiny random image and push it to org1/app:v1 (exercises /token
	// push+pull scope, chunked blob upload, and hosted CAS write).
	img, err := random.Image(1024, 2)
	require.NoError(t, err)
	wantDigest, err := img.Digest()
	require.NoError(t, err)
	wantCfg, err := img.RawConfigFile()
	require.NoError(t, err)

	ref := stack.ref(t, "org1/app", "v1")
	require.NoError(t, remote.Write(ref, img, auth, remote.WithTransport(http.DefaultTransport)),
		"authenticated push must succeed")

	// The hosted repo row must now exist, owned by the pushing API key's subject.
	r, err := stack.repos.GetRepo(context.Background(), seeded["org1"].id, "org1/app")
	require.NoError(t, err, "hosted repo row must be created on first push")
	assert.Equal(t, repo.VisibilityPrivate, repo.NormalizeVisibility(r.Visibility))
	assert.True(t, strings.HasPrefix(r.OwnerUserID, apikey.SubjectPrefix), "owner should be the API-key subject")

	// Pull it back with the same credentials; digest + config bytes must match.
	pulled, err := remote.Image(ref, auth, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "authenticated pull of the pushed image must succeed")

	gotDigest, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, gotDigest, "round-tripped manifest digest must match")

	gotCfg, err := pulled.RawConfigFile()
	require.NoError(t, err)
	assert.Equal(t, wantCfg, gotCfg, "round-tripped config bytes must match")

	// A layer must also pull back byte-identical (proves the CAS blob round-trip).
	wantLayers, err := img.Layers()
	require.NoError(t, err)
	gotLayers, err := pulled.Layers()
	require.NoError(t, err)
	require.Equal(t, len(wantLayers), len(gotLayers))
	wl0, err := wantLayers[0].Digest()
	require.NoError(t, err)
	gl0, err := gotLayers[0].Digest()
	require.NoError(t, err)
	assert.Equal(t, wl0, gl0, "layer digest must round-trip")
}

// ────────────────────────────────────────────────────────────────────────────
// Test (a) — unauthenticated pull of a private repo → 401
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryPrivatePullUnauthenticated401(t *testing.T) {
	stack, seeded := newRegistryStack(t, "org1")
	auth := basicAuth("ci@example.com", seeded["org1"].rawKey)

	img, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(stack.ref(t, "org1/secret", "v1"), img, auth,
		remote.WithTransport(http.DefaultTransport)))

	// Raw GET with no Authorization header → the Bearer challenge middleware
	// answers 401 with a WWW-Authenticate: Bearer header (drives docker login).
	resp, err := http.Get(fmt.Sprintf("http://%s/v2/org1/secret/manifests/v1", stack.host))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated private pull must be 401")
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer", "must advertise a Bearer challenge")

	// And an anonymous crane pull (which does the anonymous token dance) must
	// also fail — a private repo grants an anonymous caller no pull scope.
	_, err = remote.Image(stack.ref(t, "org1/secret", "v1"), remote.WithTransport(http.DefaultTransport))
	require.Error(t, err, "anonymous pull of a private repo must fail")
}

// ────────────────────────────────────────────────────────────────────────────
// Test (b) — after SetVisibility(public), an anonymous pull works
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryPublicRepoAnonymousPull(t *testing.T) {
	stack, seeded := newRegistryStack(t, "org1")
	auth := basicAuth("ci@example.com", seeded["org1"].rawKey)

	img, err := random.Image(512, 1)
	require.NoError(t, err)
	wantDigest, err := img.Digest()
	require.NoError(t, err)

	ref := stack.ref(t, "org1/public", "v1")
	require.NoError(t, remote.Write(ref, img, auth, remote.WithTransport(http.DefaultTransport)))

	// While private, an anonymous pull must fail.
	_, err = remote.Image(ref, remote.WithTransport(http.DefaultTransport))
	require.Error(t, err, "anonymous pull must fail while the repo is private")

	// Flip to public (org admin action, simulated by a direct store call).
	require.NoError(t, stack.repos.SetVisibility(context.Background(),
		seeded["org1"].id, "org1/public", repo.VisibilityPublic))

	// Now an anonymous pull (no credentials) must succeed via the anonymous
	// token dance, and the digest must match.
	pulled, err := remote.Image(ref, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "anonymous pull of a public repo must succeed")
	gotDigest, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, gotDigest)
}

// ────────────────────────────────────────────────────────────────────────────
// Test (c) — a push with a different org's API key is denied
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryCrossOrgPushDenied(t *testing.T) {
	stack, seeded := newRegistryStack(t, "org1", "org2")

	// org2's key attempts to push into org1's namespace → must be denied
	// (org2's principal is not a member/owner of org1).
	crossAuth := basicAuth("intruder@example.com", seeded["org2"].rawKey)
	img, err := random.Image(256, 1)
	require.NoError(t, err)

	err = remote.Write(stack.ref(t, "org1/victim", "v1"), img, crossAuth,
		remote.WithTransport(http.DefaultTransport))
	require.Error(t, err, "cross-org push must be denied")

	// No repo row should have been created in org1 from the denied push.
	_, gerr := stack.repos.GetRepo(context.Background(), seeded["org1"].id, "org1/victim")
	assert.ErrorIs(t, gerr, repo.ErrNotFound, "denied cross-org push must not create a repo")

	// Sanity: org2's key CAN push into its own namespace.
	ownAuth := basicAuth("owner@example.com", seeded["org2"].rawKey)
	require.NoError(t, remote.Write(stack.ref(t, "org2/app", "v1"), img, ownAuth,
		remote.WithTransport(http.DefaultTransport)), "same-org push must succeed")
}

// ────────────────────────────────────────────────────────────────────────────
// Test — pull-through of a non-hosted upstream name still works with the
// writable registry enabled (the Bearer gate must not break the cache proxy).
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryPullThroughStillWorks(t *testing.T) {
	// A fake upstream registry serving library/tool:latest (not a hosted org).
	fakeReg := newFakeRegistry(t, "library/tool", "latest")

	stack, _ := newRegistryStackWithOpts(t,
		[]ocihandler.Option{ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{fakeReg.asUpstream("fake")})},
		"org1",
	)

	// Anonymous pull of a NON-hosted name must succeed via the pull-through
	// cache — the /token authorizer grants pull on unknown namespaces, and the
	// OCI handler fetches from the configured upstream.
	pulled, err := remote.Image(stack.ref(t, "library/tool", "latest"), remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "anonymous pull-through of a non-hosted image must still work")
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, fakeReg.dig, got, "pull-through digest must match the upstream image")
}

// ────────────────────────────────────────────────────────────────────────────
// Test — push into an org namespace WHILE an OCI pull-through upstream is
// configured. This is the production topology (proxy an upstream AND host
// private repos). Without the owned-namespace gate, docker's first-push HEAD-
// blob existence check on the not-yet-created repo leaks to the upstream and
// returns 502/403 instead of the 404 that lets the upload proceed, breaking
// push entirely. The round trip must still succeed here.
// ────────────────────────────────────────────────────────────────────────────

func TestRegistryPushWithUpstreamConfigured(t *testing.T) {
	// An upstream that only knows library/tool:latest — it must NEVER be consulted
	// for the org-owned org1/* namespace.
	fakeReg := newFakeRegistry(t, "library/tool", "latest")
	stack, seeded := newRegistryStackWithOpts(t,
		[]ocihandler.Option{ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{fakeReg.asUpstream("fake")})},
		"org1",
	)
	auth := basicAuth("ci@example.com", seeded["org1"].rawKey)

	img, err := random.Image(1024, 2)
	require.NoError(t, err)
	wantDigest, err := img.Digest()
	require.NoError(t, err)

	ref := stack.ref(t, "org1/app", "v1")
	require.NoError(t, remote.Write(ref, img, auth, remote.WithTransport(http.DefaultTransport)),
		"push into an org namespace must succeed even with an upstream configured")

	pulled, err := remote.Image(ref, auth, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "hosted pull must succeed and must not be shadowed by the upstream")
	gotDigest, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, gotDigest)

	// Pull-through of the genuinely non-hosted name still works alongside it.
	pt, err := remote.Image(stack.ref(t, "library/tool", "latest"), remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "pull-through must coexist with hosted push")
	ptDig, err := pt.Digest()
	require.NoError(t, err)
	assert.Equal(t, fakeReg.dig, ptDig)
}

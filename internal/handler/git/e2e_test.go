package git

import (
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// e2e_test.go — the whole handler chain driven by the REAL git client:
// real client → Handler → bare mirror → git http-backend → real client.
//
// The upstream is a real git Smart HTTP server (stdlib net/http/cgi in front of
// the real `git http-backend`), NOT a stub that replays canned pkt-lines. Every
// bug fixed in this package was one that unit-green missed and a real client
// caught on contact, so the acceptance path is exercised the way clients use it.

// testLogWriter routes the upstream CGI's stderr into the test log.
//
// cgi.Handler defaults to dumping the CGI's stderr straight to os.Stderr, raw
// and without a trailing newline. git http-backend writes there routinely (e.g.
// "Service not enabled: 'receive-pack'" for a disabled service — an ordinary 403,
// not a failure), so the line lands unanchored in the test stream and glues
// itself onto whichever verdict prints next. That is how a PASSING push test's
// diagnostic came to appear underneath an unrelated "--- FAIL", and read as if
// that test had failed on its own terms. Attribute the output to the test that
// caused it.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(b []byte) (int, error) {
	w.t.Logf("upstream git http-backend stderr: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// newUpstreamGitServer serves root over git Smart HTTP and returns the server.
func newUpstreamGitServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	require.NoError(t, err)

	srv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		Stderr: testLogWriter{t},
	})
	t.Cleanup(srv.Close)
	return srv
}

// newBareUpstream publishes work as <root>/<project>.git and returns the bare
// repository's path.
func newBareUpstream(t *testing.T, root, project, work string) string {
	t.Helper()
	bare := filepath.Join(root, project+gitSuffix)
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	gitCmd(t, work, "clone", "--bare", "--quiet", "--", work, bare)
	// Serve push over Smart HTTP so a passthrough push-discovery request gets a
	// real receive-pack advertisement to assert on.
	gitCmd(t, bare, "config", "http.receivepack", "true")
	// Forge-style PR refs, which live only in the forge's namespace.
	head := gitCmd(t, work, "rev-parse", "HEAD")
	for i := 1; i <= 5; i++ {
		gitCmd(t, bare, "update-ref", "refs/pull/"+strconv.Itoa(i)+"/head", head)
	}
	return bare
}

// memMeta is an in-memory MetadataStore recording only what git uses: mutable
// TOFU pins. Every other method is unreachable from this package's code paths
// and panics rather than quietly answering — a double that invents answers to
// questions the code never asked is how a lying double starts.
type memMeta struct {
	mu      sync.Mutex
	mutable map[string]artifact.MutableEntry
}

func newMemMeta() *memMeta {
	return &memMeta{mutable: map[string]artifact.MutableEntry{}}
}

func (m *memMeta) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.mutable[key]
	if !ok {
		return nil, nil
	}
	cp := e
	return &cp, nil
}

func (m *memMeta) PutMutable(_ context.Context, e artifact.MutableEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mutable[e.Key] = e
	return nil
}

func (m *memMeta) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.mutable))
	for k := range m.mutable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memMeta) DeleteMutable(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mutable, key)
	return nil
}

func (m *memMeta) Get(context.Context, artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	panic("memMeta.Get: git never reads CAS entries")
}
func (m *memMeta) Put(context.Context, artifact.CacheEntry) error {
	panic("memMeta.Put: git never writes CAS entries")
}
func (m *memMeta) Delete(context.Context, artifact.ArtifactRef) error {
	panic("memMeta.Delete: unused by git")
}
func (m *memMeta) CacheSizeByProtocol(context.Context) (map[string]artifact.SizeStat, error) {
	panic("memMeta.CacheSizeByProtocol: unused by git")
}

func (m *memMeta) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}
func (m *memMeta) ListEntries(context.Context, string, meta.EntryFilter, meta.Page) (meta.EntryPage, error) {
	panic("memMeta.ListEntries: unused by git")
}
func (m *memMeta) SetPinned(context.Context, artifact.ArtifactRef, bool) error {
	panic("memMeta.SetPinned: unused by git")
}

var _ meta.MetadataStore = (*memMeta)(nil)

// gitProxyFixture wires: upstream git server → Handler under test → test server.
type gitProxyFixture struct {
	proxy     *httptest.Server
	mirrorDir string
	meta      *memMeta
	work      string // upstream work tree
	bare      string // the bare repo the upstream server publishes
	project   string
	host      string
}

// newGitProxyFixture wires the chain with staleAfter fixed up front.
//
// staleAfter is a CONSTRUCTION parameter, not something a test reaches in and
// changes later. mirrorStore.staleAfter is written exactly once, in
// newMirrorStore, and is read without a lock by EnsureSynced on every request —
// which is correct and race-free precisely BECAUSE production never writes it
// after construction. A test that pokes h.mirror.staleAfter on a live handler
// invents a data race that production does not have (racing the write against
// in-flight request goroutines reading it), and then blames the code under test.
// Configure it before anything serves.
func newGitProxyFixture(t *testing.T, staleAfter time.Duration) *gitProxyFixture {
	t.Helper()

	// A real upstream repository.
	work := t.TempDir()
	gitCmd(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644))
	gitCmd(t, work, "add", "README")
	gitCmd(t, work, "commit", "--quiet", "-m", "initial")
	gitCmd(t, work, "tag", "v1.0.0")

	const project = "octocat/Hello-World"
	upRoot := t.TempDir()
	bare := newBareUpstream(t, upRoot, project, work)
	upSrv := newUpstreamGitServer(t, upRoot)

	host := strings.TrimPrefix(upSrv.URL, "http://")
	mirrorDir := t.TempDir()
	ms := newMemMeta()

	h := NewHandler(
		WithMirrorDir(mirrorDir),
		WithAllowedUpstreams([]string{host}),
		WithPublicOnly(false), // the visibility probe only knows gitee/github APIs
		WithUpstreamScheme("http"),
		WithMeta(ms),
		WithSyncStaleAfter(staleAfter),
	)
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	return &gitProxyFixture{
		proxy: proxy, mirrorDir: mirrorDir, meta: ms,
		work: work, bare: bare, project: project, host: host,
	}
}

// cloneURL is the URL a real client clones from.
func (f *gitProxyFixture) cloneURL() string {
	return f.proxy.URL + "/" + f.host + "/" + f.project + gitSuffix
}

// TestRealClient_CloneThroughProxy is the acceptance path in miniature: a real
// `git clone` against the handler must produce a working checkout, and must not
// drag the forge's PR refs through the mirror.
func TestRealClient_CloneThroughProxy(t *testing.T) {
	f := newGitProxyFixture(t, time.Minute)

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), dst)

	// The client got real content.
	body, err := os.ReadFile(filepath.Join(dst, "README"))
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(body))

	// And a real tag.
	assert.Contains(t, gitCmd(t, dst, "tag", "--list"), "v1.0.0")

	// The mirror holds branches + tags, and none of the forge's PR refs.
	refs := mirrorRefs(t, filepath.Join(f.mirrorDir, f.host, f.project+gitSuffix))
	assert.Contains(t, refs, "refs/heads/master")
	assert.Contains(t, refs, "refs/tags/v1.0.0")
	for _, r := range refs {
		assert.False(t, strings.HasPrefix(r, "refs/pull/"),
			"the mirror must not carry the forge's PR namespace: %s", r)
	}
}

// TestRealClient_WarmCloneServesUpdatedRefs proves the refresh path end to end:
// a new upstream commit reaches a second real client after the staleness window.
func TestRealClient_WarmCloneServesUpdatedRefs(t *testing.T) {
	// staleAfter=0: every request re-fetches, which is what makes the second
	// clone exercise the refresh path.
	f := newGitProxyFixture(t, 0)

	first := filepath.Join(t.TempDir(), "first")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), first)

	// Upstream moves on.
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "README"), []byte("v2\n"), 0o644))
	gitCmd(t, f.work, "commit", "--quiet", "-am", "second")
	gitCmd(t, f.work, "push", "--quiet", f.bare, "master")

	second := filepath.Join(t.TempDir(), "second")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), second)

	body, err := os.ReadFile(filepath.Join(second, "README"))
	require.NoError(t, err)
	assert.Equal(t, "v2\n", string(body),
		"a warm clone after the staleness window must serve the updated ref")
}

// TestRealClient_RecordsTofuPinsAndTier proves BUG 3's premise and its fix on the
// real path: cloning through the proxy records real ref→SHA pins, and the tier
// derived from them is `tofu` — the tier PRD §G2 says this mechanism earns.
func TestRealClient_RecordsTofuPinsAndTier(t *testing.T) {
	f := newGitProxyFixture(t, time.Minute)

	repo := f.host + "/" + f.project
	assert.Empty(t, RepoTier(context.Background(), f.meta, f.mirrorDir, repo),
		"before any sync there are no pins, so no tier has been earned")

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), dst)

	// Real pins, recorded by the real clone path.
	keys := f.meta.keys()
	require.NotEmpty(t, keys, "cloning must record ref→SHA TOFU pins")
	assert.Contains(t, keys, RefTOFUKeyFor(repo, "refs/heads/master"))
	assert.Contains(t, keys, RefTOFUKeyFor(repo, "refs/tags/v1.0.0"))

	assert.Equal(t, artifact.TierTofu.String(),
		RepoTier(context.Background(), f.meta, f.mirrorDir, repo),
		"pins exist → force-push detection is live → the repo has earned exactly tofu")
}

// TestRealClient_ForcePushRaisesNonFastForwardAlert proves the guarantee the
// tofu tier actually claims: a rewritten history is detected, not silently
// accepted. Without this, "tofu" on the entry would be a label with nothing
// behind it.
func TestRealClient_ForcePushRaisesNonFastForwardAlert(t *testing.T) {
	f := newGitProxyFixture(t, 0) // re-fetch on the post-force-push sync

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), dst)

	ref := repoRef{Host: f.host, ProjectPath: f.project, Tail: "/info/refs"}
	before, err := f.meta.GetMutable(context.Background(),
		RefTOFUKeyFor(f.host+"/"+f.project, "refs/heads/master"))
	require.NoError(t, err)
	require.NotNil(t, before)

	// Upstream rewrites history and force-pushes.
	gitCmd(t, f.work, "commit", "--quiet", "--amend", "-m", "rewritten")
	gitCmd(t, f.work, "push", "--quiet", "--force", f.bare, "master")

	h := f.proxy.Config.Handler.(*Handler)
	_, syncErr := h.mirror.EnsureSynced(context.Background(), ref,
		ref.upstreamURLWithScheme("http"))
	require.NoError(t, syncErr)

	alerts := updateTOFUPins(context.Background(), f.meta, f.mirrorDir, ref, nil)

	require.NotEmpty(t, alerts, "a force-push must raise a non-fast-forward alert")
	assert.Contains(t, strings.Join(alerts, "\n"), "NON-FAST-FORWARD",
		"the tofu tier's whole claim is that a history rewrite is detected and reported")

	after, err := f.meta.GetMutable(context.Background(),
		RefTOFUKeyFor(f.host+"/"+f.project, "refs/heads/master"))
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.NotEqual(t, before.Digest, after.Digest, "the pin must move to the new SHA")
}

// TestRealClient_RecoversFromCorpseOnCloneRetry is BUG 1(b) proven with a real
// client: after a corpse is left at the mirror path, the next real clone must
// succeed rather than serve an empty repository forever.
func TestRealClient_RecoversFromCorpseOnCloneRetry(t *testing.T) {
	f := newGitProxyFixture(t, time.Minute)

	// Warm the mirror, then destroy it the way a killed clone would.
	mirror := filepath.Join(f.mirrorDir, f.host, f.project+gitSuffix)
	require.NoError(t, os.RemoveAll(mirror))
	makeCorpse(t, mirror)

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--", f.cloneURL(), dst)

	body, err := os.ReadFile(filepath.Join(dst, "README"))
	require.NoError(t, err)
	assert.Equal(t, "v1\n", string(body),
		"a real client hitting a corpse must get a re-cloned, working mirror")
}

// TestServeHTTP_PushIsPassedThroughNotMirrored guards the trust boundary: a push
// must never be served from the pull-only mirror.
func TestServeHTTP_PushIsPassedThroughNotMirrored(t *testing.T) {
	f := newGitProxyFixture(t, time.Minute)

	// receive-pack discovery is encoded in the query string, not the path tail.
	resp, err := http.Get(f.cloneURL() + "/info/refs?service=git-receive-pack")
	require.NoError(t, err)
	defer resp.Body.Close()

	// The upstream has receive-pack enabled, so a passed-through discovery gets
	// the real advertisement. The mirror never serves receive-pack, so this
	// answer could only have come from upstream.
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "service=git-receive-pack",
		"push discovery must be answered by the upstream, never from the pull-only mirror")
	assert.NoDirExists(t, filepath.Join(f.mirrorDir, f.host, f.project+gitSuffix),
		"a push-discovery request must not cause the repo to be mirrored")
}

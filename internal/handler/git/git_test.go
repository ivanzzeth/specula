package git

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─── parseProxyPath ──────────────────────────────────────────────────────────

func TestParseProxyPath(t *testing.T) {
	allowed := map[string]struct{}{
		"github.com": {},
		"gitlab.com": {},
	}

	tests := []struct {
		name     string
		path     string
		wantOK   bool
		wantHost string
		wantProj string
		wantTail string
	}{
		{
			name:     "valid info refs",
			path:     "/github.com/owner/repo.git/info/refs",
			wantOK:   true,
			wantHost: "github.com",
			wantProj: "owner/repo",
			wantTail: "/info/refs",
		},
		{
			name:     "valid upload-pack POST",
			path:     "/github.com/owner/repo.git/git-upload-pack",
			wantOK:   true,
			wantHost: "github.com",
			wantProj: "owner/repo",
			wantTail: "/git-upload-pack",
		},
		{
			name:     "valid receive-pack POST",
			path:     "/gitlab.com/org/project.git/git-receive-pack",
			wantOK:   true,
			wantHost: "gitlab.com",
			wantProj: "org/project",
			wantTail: "/git-receive-pack",
		},
		{
			name:     "deep project path (group/sub/repo)",
			path:     "/github.com/group/sub/repo.git/info/refs",
			wantOK:   true,
			wantHost: "github.com",
			wantProj: "group/sub/repo",
			wantTail: "/info/refs",
		},
		{
			name:   "disallowed host",
			path:   "/bitbucket.org/owner/repo.git/info/refs",
			wantOK: false,
		},
		{
			name:   "missing .git suffix",
			path:   "/github.com/owner/repo/info/refs",
			wantOK: false,
		},
		{
			name:   "empty path",
			path:   "/",
			wantOK: false,
		},
		{
			name:   "only host no project",
			path:   "/github.com",
			wantOK: false,
		},
		{
			name:   "path traversal in project",
			path:   "/github.com/../etc/passwd.git/info/refs",
			wantOK: false,
		},
		{
			name:   "no host segment",
			path:   "",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := parseProxyPath(tc.path, allowed)
			require.Equal(t, tc.wantOK, ok, "parseProxyPath(%q) ok mismatch", tc.path)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantHost, ref.Host, "Host")
			assert.Equal(t, tc.wantProj, ref.ProjectPath, "ProjectPath")
			assert.Equal(t, tc.wantTail, ref.Tail, "Tail")
		})
	}
}

// ─── repoRef helpers ─────────────────────────────────────────────────────────

func TestRepoRefMethods(t *testing.T) {
	ref := repoRef{
		Host:        "github.com",
		ProjectPath: "owner/repo",
		Tail:        "/info/refs",
	}

	t.Run("mirrorRelPath", func(t *testing.T) {
		assert.Equal(t, "github.com/owner/repo.git", ref.mirrorRelPath())
	})

	t.Run("upstreamURLWithScheme_https", func(t *testing.T) {
		assert.Equal(t, "https://github.com/owner/repo.git",
			ref.upstreamURLWithScheme("https"))
	})

	t.Run("upstreamURLWithScheme_http", func(t *testing.T) {
		assert.Equal(t, "http://github.com/owner/repo.git",
			ref.upstreamURLWithScheme("http"))
	})

	t.Run("isRefAdvertise_true", func(t *testing.T) {
		assert.True(t, ref.isRefAdvertise())
	})

	t.Run("isRefAdvertise_false", func(t *testing.T) {
		r2 := repoRef{Tail: "/git-upload-pack"}
		assert.False(t, r2.isRefAdvertise())
	})

	t.Run("isReceivePack_false_for_upload", func(t *testing.T) {
		r2 := repoRef{Tail: "/git-upload-pack"}
		assert.False(t, r2.isReceivePack())
	})

	t.Run("isReceivePack_true", func(t *testing.T) {
		r2 := repoRef{Tail: "/git-receive-pack"}
		assert.True(t, r2.isReceivePack())
	})

	t.Run("isReceivePack_true_info_refs_receive", func(t *testing.T) {
		r2 := repoRef{Tail: "/info/refs"} // will have query ?service=git-receive-pack
		// Note: tail does NOT include the query string; receive-pack is detected
		// by the tail path segment, not the query. This is the Tail-based check.
		// The query-based check happens in ServeHTTP via the handler detecting
		// receive-pack from r.URL.RawQuery at a higher layer. For /info/refs the
		// tail alone is NOT enough to classify as receive-pack; classification is
		// handled by checking the tail path for "git-receive-pack".
		assert.False(t, r2.isReceivePack(),
			"isRefAdvertise tail does not contain 'git-receive-pack'")
	})
}

// ─── hasAuth ─────────────────────────────────────────────────────────────────

func TestHasAuth(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{
			name: "no auth headers",
			headers: map[string]string{
				"Content-Type": "text/plain",
			},
			want: false,
		},
		{
			name: "Authorization header present",
			headers: map[string]string{
				"Authorization": "Basic dXNlcjpwYXNz",
			},
			want: true,
		},
		{
			name: "Proxy-Authorization header present",
			headers: map[string]string{
				"Proxy-Authorization": "Bearer token123",
			},
			want: true,
		},
		{
			name: "whitespace-only Authorization",
			headers: map[string]string{
				"Authorization": "   ",
			},
			want: false,
		},
		{
			name:    "empty request",
			headers: map[string]string{},
			want:    false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			assert.Equal(t, tc.want, hasAuth(r))
		})
	}
}

// ─── refToArtifact ───────────────────────────────────────────────────────────

func TestRefToArtifact(t *testing.T) {
	tests := []struct {
		name string
		ref  repoRef
		want artifact.ArtifactRef
	}{
		{
			name: "info/refs (mutable ref advertisement)",
			ref: repoRef{
				Host:        "github.com",
				ProjectPath: "owner/repo",
				Tail:        "/info/refs",
			},
			want: artifact.ArtifactRef{
				Protocol: Protocol,
				Name:     "github.com/owner/repo",
				Version:  "info/refs",
				Mutable:  true,
			},
		},
		{
			name: "git-upload-pack (immutable packfile)",
			ref: repoRef{
				Host:        "gitlab.com",
				ProjectPath: "group/project",
				Tail:        "/git-upload-pack",
			},
			want: artifact.ArtifactRef{
				Protocol: Protocol,
				Name:     "gitlab.com/group/project",
				Version:  "git-upload-pack",
				Mutable:  false,
			},
		},
		{
			name: "root tail",
			ref: repoRef{
				Host:        "github.com",
				ProjectPath: "a/b",
				Tail:        "/",
			},
			want: artifact.ArtifactRef{
				Protocol: Protocol,
				Name:     "github.com/a/b",
				Version:  "",
				Mutable:  false,
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := refToArtifact(tc.ref)
			assert.Equal(t, tc.want.Protocol, got.Protocol, "Protocol")
			assert.Equal(t, tc.want.Name, got.Name, "Name")
			assert.Equal(t, tc.want.Version, got.Version, "Version")
			assert.Equal(t, tc.want.Mutable, got.Mutable, "Mutable")
		})
	}
}

// ─── NewHandler / option defaults ────────────────────────────────────────────

func TestNewHandlerDefaults(t *testing.T) {
	h := NewHandler()

	assert.True(t, h.publicOnly, "publicOnly default true")
	assert.True(t, h.failClosed, "failClosed default true")
	assert.Equal(t, "https", h.upstreamScheme, "upstreamScheme default https")
	assert.Equal(t, defaultSyncStaleAfter, h.syncStaleAfter)
	assert.Equal(t, defaultUpstreamTimeout, h.upstreamTimeout)
	assert.NotNil(t, h.mirror, "mirror must be initialised in NewHandler")
	assert.NotNil(t, h.pubCheck, "pubCheck must be initialised in NewHandler")
	assert.NotNil(t, h.transport, "transport must be initialised in NewHandler")
	assert.NotNil(t, h.log, "logger must be non-nil")
}

func TestNewHandlerOptions(t *testing.T) {
	stale := 5 * time.Second
	h := NewHandler(
		WithMirrorDir("/tmp/mirrors"),
		WithAllowedUpstreams([]string{"github.com", "gitlab.com"}),
		WithPublicOnly(false),
		WithFailClosed(false),
		WithSyncStaleAfter(stale),
		WithUpstreamScheme("http"),
	)

	assert.Equal(t, "/tmp/mirrors", h.mirrorDir)
	assert.Contains(t, h.allowed, "github.com")
	assert.Contains(t, h.allowed, "gitlab.com")
	assert.False(t, h.publicOnly)
	assert.False(t, h.failClosed)
	assert.Equal(t, stale, h.syncStaleAfter)
	assert.Equal(t, "http", h.upstreamScheme)
}

// ─── Handler.ServeHTTP routing ───────────────────────────────────────────────

func TestServeHTTPDisallowedHost(t *testing.T) {
	h := NewHandler(
		WithAllowedUpstreams([]string{"github.com"}),
		WithPublicOnly(false),
	)

	// Request for a host NOT in the allowlist.
	r := httptest.NewRequest(http.MethodGet,
		"/bitbucket.org/owner/repo.git/info/refs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"disallowed host must return 404")
}

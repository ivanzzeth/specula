package verify

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

func TestHTTPMirrorDigestFetcher_NPMIntegrity(t *testing.T) {
	const integrity = "sha512-AbCdEfGhIjKlMnOpQrStUvWxYz1234567890+/ABCDEFGHIJKLMNOPQRSTUVWXYZ=="
	packument := `{
		"name":"left-pad",
		"versions":{
			"1.3.0":{"dist":{"integrity":"` + integrity + `"}}
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/left-pad", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(packument))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	got, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm",
		Name:     "left-pad",
		Version:  "left-pad-1.3.0.tgz",
	})
	require.NoError(t, err)
	assert.Equal(t, integrity, got)
}

func TestHTTPMirrorDigestFetcher_NPMIntegrity_MissingVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"x","versions":{}}`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "x", Version: "x-1.0.0.tgz",
	})
	require.Error(t, err)
}

func TestHTTPMirrorDigestFetcher_CargoChecksum(t *testing.T) {
	const cksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	index := `{"name":"serde","vers":"1.0.0","cksum":"` + cksum + `","deps":[]}
{"name":"serde","vers":"1.0.1","cksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","deps":[]}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/se/rd/serde", r.URL.Path)
		_, _ = w.Write([]byte(index))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	got, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "cargo", Name: "serde", Version: "1.0.0",
	})
	require.NoError(t, err)
	assert.Equal(t, cksum, got)
}

func TestHTTPMirrorDigestFetcher_UnsupportedProtocol(t *testing.T) {
	unsupported := []string{"gomod", "apt", "helm", "tarball", "git"}
	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	mirror := ConsensusMirror{Name: "test", BaseURL: "http://example.com"}

	for _, proto := range unsupported {
		t.Run(proto, func(t *testing.T) {
			ref := artifact.ArtifactRef{
				Protocol: proto,
				Name:     "example",
				Version:  "1.0.0",
			}
			_, err := f.FetchDigest(t.Context(), mirror, ref)
			require.Error(t, err, "%s: unsupported protocol must return error", proto)
			assert.Contains(t, err.Error(), proto)
		})
	}
}

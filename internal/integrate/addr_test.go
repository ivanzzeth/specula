package integrate

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeAddr_DefaultHTTPS(t *testing.T) {
	got, err := normalizeAddr("")
	require.NoError(t, err)
	assert.Equal(t, DefaultAddr, got)
	assert.True(t, len(got) >= 8 && got[:8] == "https://")
}

func TestNormalizeAddr_RejectsBadScheme(t *testing.T) {
	_, err := normalizeAddr("ftp://127.0.0.1:7732")
	require.Error(t, err)
}

func TestResolveDataPlaneAddr_SkipProbeKeepsHTTP(t *testing.T) {
	got, up, err := resolveDataPlaneAddr("http://127.0.0.1:7732", true)
	require.NoError(t, err)
	assert.False(t, up)
	assert.Equal(t, "http://127.0.0.1:7732", got)
}

func TestResolveDataPlaneAddr_AutoUpgradeTLSOnly(t *testing.T) {
	// TLS-only listener serving registry-ish responses over HTTPS only.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	httpAddr := "http://" + srv.Listener.Addr().String()
	got, up, err := resolveDataPlaneAddr(httpAddr, false)
	require.NoError(t, err)
	assert.True(t, up, "TLS-only port must auto-upgrade")
	assert.Equal(t, "https://"+srv.Listener.Addr().String(), got)
}

func TestResolveDataPlaneAddr_KeepsWorkingHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	got, up, err := resolveDataPlaneAddr(srv.URL, false)
	require.NoError(t, err)
	assert.False(t, up)
	assert.Equal(t, srv.URL, got)
}

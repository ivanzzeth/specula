package upstream

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetch_Upstream404_CarriesTypedStatus is the mechanism-level regression
// test for BUG 1. When a REAL upstream answers a fetch with a definitive
// non-retryable status (404/410/403), Fetch must return an error that carries
// that HTTP status in a typed, inspectable form, so a data-plane handler can
// preserve the GOPROXY-protocol meaning (404/410 = "does not exist") instead of
// flattening every upstream error to 502.
//
// Before the fix the error is an opaque fmt.Errorf string with no status, so
// errors.As below fails — proving the code cannot tell a clean 404 from a
// transport failure.
func TestFetch_Upstream404_CarriesTypedStatus(t *testing.T) {
	cases := []int{http.StatusNotFound, http.StatusGone, http.StatusForbidden}
	for _, code := range cases {
		srv := statusServer(t, code)
		t.Cleanup(srv.Close)

		c := testClient(1)
		_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
			[]Upstream{{Name: "only", BaseURL: srv.URL, Priority: 1}})
		require.Error(t, err)

		var se *StatusError
		require.Truef(t, errors.As(err, &se),
			"Fetch error for HTTP %d must carry a typed *StatusError (got %v)", code, err)
		assert.Equal(t, code, se.StatusCode, "typed status must equal the upstream status")
	}
}

// TestFetch_ConnRefused_NoTypedStatus proves the other side of the distinction:
// a genuine transport failure (connection refused) must NOT carry a StatusError,
// so the handler keeps mapping it to 502 rather than a fake 404.
func TestFetch_ConnRefused_NoTypedStatus(t *testing.T) {
	// Start then close a server so its address refuses connections.
	dead := statusServer(t, http.StatusOK)
	deadURL := dead.URL
	dead.Close()

	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{{Name: "dead", BaseURL: deadURL, Priority: 1}})
	require.Error(t, err)

	var se *StatusError
	assert.Falsef(t, errors.As(err, &se),
		"a transport failure must NOT carry a StatusError (got %v)", err)
}

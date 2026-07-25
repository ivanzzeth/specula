package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resumeTestClient is a fallbackClient with short idle / no Client.Timeout so
// resume behaviour is testable without multi-second sleeps.
func resumeTestClient() *fallbackClient {
	return &fallbackClient{
		http:              newUpstreamHTTPClient(),
		blocker:           newBlockTrackerWith(defaultMaxFailures, defaultBlockDuration),
		maxAttempts:       3, // allow 401 dance + Range retry on resume
		backoffBase:       time.Millisecond,
		tokens:            make(map[string]tokenEntry),
		idleBodyTimeout:   80 * time.Millisecond,
		maxResumeAttempts: 8,
	}
}

func ociBlobRef(name, digest string) artifact.ArtifactRef {
	return artifact.ArtifactRef{Protocol: "oci", Name: name, Digest: digest}
}

// writePrefixThenDrop delivers prefix bytes to the client, flushes, then
// forcibly closes the connection so the client sees a mid-stream failure with
// offset > 0 (enabling Range resume).
func writePrefixThenDrop(t *testing.T, w http.ResponseWriter, full string, prefixLen int) {
	t.Helper()
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(full[:prefixLen]))
	require.NoError(t, err)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Give the client transport a moment to observe the flushed bytes.
	time.Sleep(30 * time.Millisecond)
	if hj, ok := w.(http.Hijacker); ok {
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}

// TestFetch_ResumeAfterMidStreamClose covers the nginx-class failure: upstream
// delivers a prefix then drops the connection; the client must Range-resume
// from the byte offset already read and assemble the full blob.
func TestFetch_ResumeAfterMidStreamClose(t *testing.T) {
	const full = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	prefixLen := 10
	var gets atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		rangeHdr := r.Header.Get("Range")
		if n == 1 {
			assert.Empty(t, rangeHdr, "first GET must be full-body")
			writePrefixThenDrop(t, w, full, prefixLen)
			return
		}
		require.Equal(t, fmt.Sprintf("bytes=%d-", prefixLen), rangeHdr)
		rest := full[prefixLen:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefixLen, len(full)-1, len(full)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(rest))
	}))
	defer srv.Close()

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), ociBlobRef("library/nginx", "sha256:dead"),
		[]Upstream{{Name: "mirror", BaseURL: srv.URL, Priority: 1}})
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))
	assert.GreaterOrEqual(t, gets.Load(), int64(2), "must have issued a Range resume GET")
}

// TestFetch_LongBodyExceedsFormerClientTimeout proves there is no absolute
// Client.Timeout covering the body: headers arrive quickly, body streams longer
// than ResponseHeaderTimeout while bytes keep arriving.
func TestFetch_LongBodyExceedsFormerClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			_, _ = w.Write([]byte("chunk-"))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := resumeTestClient()
	tr := c.http.Transport.(*http.Transport)
	tr.ResponseHeaderTimeout = 30 * time.Millisecond
	c.idleBodyTimeout = 200 * time.Millisecond

	start := time.Now()
	body, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1"),
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})
	require.NoError(t, err)
	defer body.Close()
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.Equal(t, strings.Repeat("chunk-", 8), string(got))
	assert.Greater(t, elapsed, 200*time.Millisecond, "body must be allowed to outlive header timeout")
	assert.Equal(t, time.Duration(0), c.http.Timeout, "Client.Timeout must stay unset")
}

// TestFetch_ResumeRenewsBearerOn401: a Range resume that gets 401 must refresh
// the token and continue (自动续期).
func TestFetch_ResumeRenewsBearerOn401(t *testing.T) {
	const full = "payload-bytes-for-resume-auth-test!!"
	prefixLen := 12
	var blobGets, tokenGets atomic.Int64

	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenGets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"spck_fresh","expires_in":300}`)
			return
		}
		cur := blobGets.Add(1)
		auth := r.Header.Get("Authorization")
		rangeHdr := r.Header.Get("Range")
		if cur == 1 {
			writePrefixThenDrop(t, w, full, prefixLen)
			return
		}
		if rangeHdr != "" && auth == "" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:library/nginx:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth != "Bearer spck_fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := full[prefixLen:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefixLen, len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(rest))
	})
	defer srv.Close()

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), ociBlobRef("library/nginx", "sha256:ab"),
		[]Upstream{{Name: "hub", BaseURL: srv.URL, Priority: 1}})
	require.NoError(t, err)
	defer body.Close()
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))
	assert.GreaterOrEqual(t, tokenGets.Load(), int64(1), "must renew bearer on resume 401")
}

// TestFetch_NoRangeSupportRestartsFromZero: mid-stream drop then a 200 full
// body on the Range request must signal restart-from-0 so the caller can
// truncate quarantine and re-hash.
func TestFetch_NoRangeSupportRestartsFromZero(t *testing.T) {
	const full = "FULL-CONTENT-NO-RANGE-SUPPORT-OK"
	prefixLen := 8
	var gets atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		if n == 1 {
			writePrefixThenDrop(t, w, full, prefixLen)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v2"),
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})
	require.NoError(t, err)
	defer body.Close()

	var out []byte
	buf := make([]byte, 64)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, ErrRestartFromBeginning) {
			out = out[:0]
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	assert.Equal(t, full, string(out))
	sum := sha256.Sum256([]byte(full))
	_ = hex.EncodeToString(sum[:])
}

// TestFetch_ContextCancelStopsResume: cancelling ctx must abort without spinning.
func TestFetch_ContextCancelStopsResume(t *testing.T) {
	blockResume := make(chan struct{})
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		if n == 1 {
			writePrefixThenDrop(t, w, "partial-data!!!!", 7)
			return
		}
		select {
		case <-blockResume:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer srv.Close()

	c := resumeTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	body, _, err := c.Fetch(ctx, tarballRef("pkg", "v3"),
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})
	require.NoError(t, err)
	defer body.Close()

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(body)
		done <- err
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled) || isContextError(err), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("ReadAll did not return after ctx cancel")
	}
	close(blockResume)
}

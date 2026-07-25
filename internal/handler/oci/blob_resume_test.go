package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// TestBlobColdFill_SurvivesClientCancel is the longhorn ImagePullBackOff root
// cause in miniature: containerd cancels while Specula is still quarantining.
// Fill must detach and finish so the next pull hits CAS instead of restarting
// from byte 0.
func TestBlobColdFill_SurvivesClientCancel(t *testing.T) {
	blobData := bytes.Repeat([]byte("L"), 256*1024)
	sum := sha256.Sum256(blobData)
	bDigest := "sha256:" + hex.EncodeToString(sum[:])

	gate := make(chan struct{})
	var upstreamHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			http.NotFound(w, r)
			return
		}
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", bDigest)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write(blobData[:4096])
			f.Flush()
			<-gate
			_, _ = w.Write(blobData[4096:])
			return
		}
		_, _ = w.Write(blobData)
	}))
	t.Cleanup(up.Close)

	qdir := t.TempDir()
	fc := newStoringFakeCache()
	h := NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake", BaseURL: up.URL, Priority: 1},
		}),
		WithQuarantineDir(qdir),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/library/img/blobs/"+bDigest, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		errCh <- nil
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-errCh

	close(gate)
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/img",
		Version:  bDigest,
		Digest:   bDigest,
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, err := fc.Lookup(context.Background(), ref)
		require.NoError(t, err)
		if entry != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CAS was not populated after client cancel; fill did not outlive the request")
		}
		time.Sleep(20 * time.Millisecond)
	}

	hitsBefore := upstreamHits.Load()
	resp, err := http.Get(srv.URL + "/v2/library/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
	assert.Equal(t, hitsBefore, upstreamHits.Load(),
		"second GET must be served from CAS; another upstream hit means fill was aborted/lost")
}

// TestBlobColdFill_RangeResumeAcrossAbort: first fill aborted with partial on
// disk; second fill sends Range and completes without re-downloading prefix.
func TestBlobColdFill_RangeResumeAcrossAbort(t *testing.T) {
	blobData := bytes.Repeat([]byte("R"), 64*1024)
	sum := sha256.Sum256(blobData)
	bDigest := "sha256:" + hex.EncodeToString(sum[:])

	var sawRange atomic.Bool
	var fullGETs atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			http.NotFound(w, r)
			return
		}
		rng := r.Header.Get("Range")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", bDigest)
		if strings.HasPrefix(rng, "bytes=") {
			sawRange.Store(true)
			off := parseRangeStart(rng)
			w.Header().Set("Content-Range",
				"bytes "+strconv.Itoa(off)+"-"+strconv.Itoa(len(blobData)-1)+"/"+strconv.Itoa(len(blobData)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(blobData[off:])
			return
		}
		fullGETs.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(blobData)
	}))
	t.Cleanup(up.Close)

	qdir := t.TempDir()
	partial := cache.DurablePartialPath(qdir, bDigest)
	require.NoError(t, os.WriteFile(partial, blobData[:8192], 0o600))

	fc := newStoringFakeCache()
	h := NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake", BaseURL: up.URL, Priority: 1},
		}),
		WithQuarantineDir(qdir),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/library/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
	assert.True(t, sawRange.Load(), "fill must Range-resume from durable partial offset")
	assert.Equal(t, int32(0), fullGETs.Load(), "must not re-fetch the whole blob when partial exists")
}

func parseRangeStart(rng string) int {
	s := strings.TrimPrefix(rng, "bytes=")
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

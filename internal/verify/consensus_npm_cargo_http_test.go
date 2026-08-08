package verify

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ── npm packument size / Accept negotiation ─────────────────────────────────
//
// Real-world failure this pins down: `npm ci` of a React app failed every
// tarball with HTTP 502 "tarball failed integrity verification". The consensus
// tier reported "0 of 2 mirrors responded", which reads like an outage — but
// all three mirrors had answered fine. react's FULL packument is ~6.9 MB
// (thousands of versions, each with README/metadata), so io.LimitReader at the
// shared 4 MiB maxIndexBytes truncated the JSON mid-document, json.Unmarshal
// failed, and every mirror was recorded as "no dist.integrity for version X".
//
// Two independent defects, so two independent fixes:
//   1. We never asked for npm's abbreviated packument
//      (Accept: application/vnd.npm.install-v1+json), which exists precisely
//      for this: same dist.integrity, without the README bulk (6.9 MB → 2.9 MB
//      for react).
//   2. The cap itself was the generic 4 MiB. Abbreviated is smaller but NOT
//      bounded by it, and mirrors are free to ignore the Accept header —
//      huaweicloud returns the full 6.9 MB document either way. Asking nicely
//      is not a size guarantee.

// The fetcher must request the abbreviated packument. A mirror that honours it
// (npmjs, npmmirror) then sends a fraction of the bytes.
func TestHTTPMirrorDigestFetcher_NPM_RequestsAbbreviatedPackument(t *testing.T) {
	const integrity = "sha512-abbreviated0000000000000000000000000000000000000000000000000000000000000000000000000000=="
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"react","versions":{"19.0.5":{"dist":{"integrity":"` + integrity + `"}}}}`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	got, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "react", Version: "react-19.0.5.tgz",
	})
	require.NoError(t, err)
	assert.Equal(t, integrity, got)
	assert.Contains(t, gotAccept, "application/vnd.npm.install-v1+json",
		"npm's abbreviated packument must be requested — the full document is several MB of README we never read")
}

// A packument past the OLD shared 4 MiB cap must still be parsed. This is the
// react case: a mirror that ignores Accept and returns the full document.
func TestHTTPMirrorDigestFetcher_NPM_PackumentPastOldCap_Success(t *testing.T) {
	const integrity = "sha512-pastoldcap00000000000000000000000000000000000000000000000000000000000000000000000000000=="

	// Build a document whose target version sits AFTER >4 MiB of other
	// versions — npm orders versions oldest-first, so the newest release (the
	// one being installed) is exactly the one truncation loses.
	var buf bytes.Buffer
	buf.WriteString(`{"name":"react","versions":{`)
	filler := `"0.0.%d":{"dist":{"integrity":"sha512-` + strings.Repeat("f", 86) + `=="}},`
	for i := 0; buf.Len() < maxIndexBytes+(1<<20); i++ { // exceed old 4 MiB by 1 MiB
		fmt.Fprintf(&buf, filler, i)
	}
	buf.WriteString(`"19.0.5":{"dist":{"integrity":"` + integrity + `"}}}}`)

	require.Greater(t, buf.Len(), maxIndexBytes, "fixture must exceed the old 4 MiB cap")
	require.Less(t, int64(buf.Len()), int64(maxNPMPackumentBytes), "fixture must stay under the stream guard")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(20 * time.Second)
	got, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "react", Version: "react-19.0.5.tgz",
	})
	require.NoError(t, err,
		"a packument past the old 4 MiB cap must still be parsed — this is the react regression")
	assert.Equal(t, integrity, got)
}

// Past even the new cap, the error must say "truncated / inconclusive" and NOT
// read like "this version does not exist". Those two demand opposite responses
// from an operator, and conflating them is what made the react incident take
// three wrong diagnoses (too-new package / poisoned cache / publish-date
// blocking) before the actual cause.
func TestHTTPMirrorDigestFetcher_NPM_PackumentExceedsNewCap_TruncatedError(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"name":"react","versions":{`)
	filler := `"0.0.%d":{"dist":{"integrity":"sha512-` + strings.Repeat("f", 86) + `=="}},`
	for i := 0; int64(buf.Len()) < int64(maxNPMPackumentBytes)+(1<<20); i++ {
		fmt.Fprintf(&buf, filler, i)
	}
	buf.WriteString(`"19.0.5":{"dist":{"integrity":"sha512-never-reached=="}}}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(30 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "react", Version: "react-19.0.5.tgz",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated",
		"an oversized packument must be reported as truncated/inconclusive, not as a missing version")
}

// The genuinely-absent case must stay distinguishable from truncation: same
// operator, opposite next step (give up vs raise the cap / retry).
func TestHTTPMirrorDigestFetcher_NPM_MissingVersion_IsNotReportedAsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"react","versions":{"18.0.0":{"dist":{"integrity":"sha512-x=="}}}}`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "react", Version: "react-19.0.5.tgz",
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "truncated",
		"a version that is really absent must not be blamed on truncation")
}

// Cargo shares npm's failure shape (cap + indistinguishable "not found"), so it
// gets the same distinction. The cap itself is deliberately NOT raised: the
// largest real sparse index measured ~1.3 MB against 4 MiB, and NDJSON has no
// README bulk to close that gap. Only the diagnosis was missing.
func TestHTTPMirrorDigestFetcher_Cargo_IndexExceedsCap_TruncatedError(t *testing.T) {
	var buf bytes.Buffer
	line := `{"name":"serde","vers":"0.0.%d","cksum":"` + strings.Repeat("a", 64) + `","deps":[]}` + "\n"
	for i := 0; buf.Len() < maxIndexBytes+(1<<20); i++ {
		fmt.Fprintf(&buf, line, i)
	}
	fmt.Fprintf(&buf, `{"name":"serde","vers":"1.0.0","cksum":"%s","deps":[]}`+"\n", strings.Repeat("b", 64))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(20 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "cargo", Name: "serde", Version: "1.0.0",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated",
		"an oversized cargo index must be reported as truncated, not as a missing version")
}

func TestHTTPMirrorDigestFetcher_Cargo_MissingVersion_IsNotReportedAsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"serde","vers":"0.9.0","cksum":"` + strings.Repeat("a", 64) + `","deps":[]}` + "\n"))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(5 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "cargo", Name: "serde", Version: "1.0.0",
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "truncated")
}

// A mirror that ignores Accept and serves the FULL packument can exceed any
// fixed cap: vite's full document measured ~38.9 MB from huaweicloud, versus
// ~2.2 MB abbreviated from the mirrors that honour the header. Raising the cap
// to cover the worst-behaved mirror would let that mirror dictate everyone's
// memory ceiling, so instead the parser streams and stops at the target
// version — peak memory is bounded by the version entry, not the document.
func TestHTTPMirrorDigestFetcher_NPM_HugePackument_StreamsWithoutBufferingAll(t *testing.T) {
	const integrity = "sha512-streamed000000000000000000000000000000000000000000000000000000000000000000000000000000=="

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream ~40 MB of preceding versions, then the target. Written
		// incrementally so the fixture itself does not need 40 MB resident.
		_, _ = w.Write([]byte(`{"name":"vite","versions":{`))
		filler := fmt.Sprintf(`"0.0.%%d":{"dist":{"integrity":"sha512-%s=="}},`, strings.Repeat("f", 86))
		for written, i := 0, 0; written < 40<<20; i++ {
			n, err := fmt.Fprintf(w, filler, i)
			if err != nil {
				return
			}
			written += n
		}
		_, _ = w.Write([]byte(`"8.0.10":{"dist":{"integrity":"` + integrity + `"}}}}`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(60 * time.Second)
	got, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "vite", Version: "vite-8.0.10.tgz",
	})
	require.NoError(t, err,
		"a full packument past the byte cap must still resolve — a badly-behaved mirror must not be able to veto the quorum")
	assert.Equal(t, integrity, got)
}

// Streaming must not lose the ability to say "this version is genuinely absent"
// — that verdict is what lets consensus distinguish a real disagreement from an
// infrastructure problem.
func TestHTTPMirrorDigestFetcher_NPM_HugePackument_MissingVersionStillReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"vite","versions":{`))
		filler := fmt.Sprintf(`"0.0.%%d":{"dist":{"integrity":"sha512-%s=="}},`, strings.Repeat("f", 86))
		for written, i := 0, 0; written < 8<<20; i++ {
			n, err := fmt.Fprintf(w, filler, i)
			if err != nil {
				return
			}
			written += n
		}
		_, _ = w.Write([]byte(`"1.2.3":{"dist":{"integrity":"sha512-other=="}}}}`))
	}))
	defer srv.Close()

	f := NewHTTPMirrorDigestFetcher(60 * time.Second)
	_, err := f.FetchDigest(t.Context(), ConsensusMirror{Name: "m", BaseURL: srv.URL}, artifact.ArtifactRef{
		Protocol: "npm", Name: "vite", Version: "vite-8.0.10.tgz",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no dist.integrity",
		"a version absent from a large document must still be reported as absent, not as truncation")
}

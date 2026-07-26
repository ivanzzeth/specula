package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
)

// ErrRestartFromBeginning is returned once from a resumingReader when a Range
// resume was answered with a full 200 body (upstream ignored Range). Callers
// that hash while writing (quarantine) must truncate and reset their hasher
// before continuing to Read.
var ErrRestartFromBeginning = errors.New("upstream: restart download from beginning")

var errIdleBodyTimeout = errors.New("upstream: idle body timeout")

const (
	// defaultResponseHeaderTimeout is how long we wait for response headers
	// after the connection is up. It deliberately does NOT cover body transfer.
	defaultResponseHeaderTimeout = 30 * time.Second

	// fastResponseHeaderTimeout is the non-final-mirror header wait. CN CDNs
	// that never answer should fail over in seconds, not hang for 30s.
	fastResponseHeaderTimeout = 8 * time.Second

	// defaultIdleBodyTimeout is how long a body Read may block with no bytes
	// before we close the connection and attempt a Range resume.
	defaultIdleBodyTimeout = 60 * time.Second

	// defaultMaxResumeAttempts caps mid-stream Range retries per Fetch.
	defaultMaxResumeAttempts = 16

	defaultDialTimeout         = 30 * time.Second
	fastDialTimeout            = 5 * time.Second
	defaultTLSHandshakeTimeout = 30 * time.Second
	fastTLSHandshakeTimeout    = 5 * time.Second
)

// newUpstreamHTTPClient builds the patient HTTP client for last-hop / sole
// upstream fetches. Client.Timeout is intentionally unset: an absolute deadline
// that includes the response body is what killed multi-minute OCI layer pulls
// (Client.Timeout while reading body → quarantine → 502). Safety nets live on
// the Transport (dial / TLS / response headers) and on the idle body reader.
func newUpstreamHTTPClient() *http.Client {
	return newUpstreamHTTPClientWith(defaultDialTimeout, defaultTLSHandshakeTimeout, defaultResponseHeaderTimeout)
}

// newUpstreamHTTPClientFast builds the fail-fast client for non-final mirrors:
// short dial/TLS/header waits so a dead CDN (DaoCloud→R2) yields to the next
// mirror in seconds rather than ~30s × attempts.
func newUpstreamHTTPClientFast() *http.Client {
	return newUpstreamHTTPClientWith(fastDialTimeout, fastTLSHandshakeTimeout, fastResponseHeaderTimeout)
}

func newUpstreamHTTPClientWith(dial, tlsHS, headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dial,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   32,
			MaxConnsPerHost:       64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   tlsHS,
			ResponseHeaderTimeout: headerTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// resumingReader wraps an upstream response body and transparently Range-resumes
// on mid-stream transport / idle failures. It first retries the pinned upstream,
// then falls through later mirrors in the chain without closing the caller's
// ReadCloser (quarantine / client connection stays open).
type resumingReader struct {
	client *fallbackClient
	ctx    context.Context
	ref    artifact.ArtifactRef
	up     Upstream
	chain  []Upstream // full ordered chain for cross-upstream fallthrough
	ropts  requestOpts

	body   io.ReadCloser
	offset int64

	resumes int
	// announceRestart is set when a resume returned HTTP 200 full body; the
	// next Read returns ErrRestartFromBeginning once, then serves the new body.
	announceRestart bool

	idleTimeout time.Duration
	maxResumes  int

	closed bool
}

func (c *fallbackClient) wrapResuming(
	ctx context.Context,
	ref artifact.ArtifactRef,
	up Upstream,
	chain []Upstream,
	ropts requestOpts,
	body io.ReadCloser,
) io.ReadCloser {
	idle := c.idleBodyTimeout
	if idle <= 0 {
		idle = defaultIdleBodyTimeout
	}
	maxR := c.maxResumeAttempts
	if maxR <= 0 {
		maxR = defaultMaxResumeAttempts
	}
	if len(chain) == 0 {
		chain = []Upstream{up}
	}
	return &resumingReader{
		client:      c,
		ctx:         ctx,
		ref:         ref,
		up:          up,
		chain:       append([]Upstream(nil), chain...),
		ropts:       ropts,
		body:        newIdleReadCloser(body, idle),
		idleTimeout: idle,
		maxResumes:  maxR,
	}
}

func (r *resumingReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, net.ErrClosed
	}
	if r.announceRestart {
		r.announceRestart = false
		r.offset = 0
		return 0, ErrRestartFromBeginning
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.body == nil {
		return 0, io.EOF
	}

	n, err := r.body.Read(p)
	r.offset += int64(n)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	if !isResumableReadError(err) {
		return n, err
	}
	if resumeErr := r.resume(); resumeErr != nil {
		if n > 0 {
			return n, nil
		}
		return 0, resumeErr
	}
	if n > 0 {
		return n, nil
	}
	return r.Read(p)
}

func (r *resumingReader) resume() error {
	if r.resumes >= r.maxResumes {
		return fmt.Errorf("upstream %s: exceeded %d resume attempts at offset %d",
			r.up.Name, r.maxResumes, r.offset)
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.resumes++
	if r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}

	from := r.offset
	candidates := r.resumeCandidates()
	var lastErr error
	for i, up := range candidates {
		if r.client.blocker.isBlocked(up.Name) {
			continue
		}
		remainingAfter := len(candidates) - 1 - i
		body, meta, err := r.fetchFrom(up, from, remainingAfter)
		if err != nil {
			lastErr = err
			metrics.RecordUpstreamFailover(r.ref.Protocol, up.Name, failoverReason(err))
			continue
		}
		switch {
		case meta.StatusCode == http.StatusPartialContent && from > 0:
			r.up = up
			r.body = newIdleReadCloser(body, r.idleTimeout)
			return nil
		case meta.StatusCode == http.StatusOK && from > 0:
			r.up = up
			r.body = newIdleReadCloser(body, r.idleTimeout)
			r.announceRestart = true
			return nil
		case meta.StatusCode >= 200 && meta.StatusCode < 300 && from == 0:
			r.up = up
			r.body = newIdleReadCloser(body, r.idleTimeout)
			return nil
		default:
			_ = body.Close()
			lastErr = fmt.Errorf("upstream %s: resume unexpected status %d at offset %d",
				up.Name, meta.StatusCode, from)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("upstream: resume at offset %d: no healthy upstream", from)
	}
	return lastErr
}

// resumeCandidates returns the pinned upstream first, then every other mirror
// in the original chain (cross-upstream fallthrough after pinned death).
func (r *resumingReader) resumeCandidates() []Upstream {
	out := make([]Upstream, 0, len(r.chain)+1)
	out = append(out, r.up)
	seen := map[string]struct{}{r.up.Name: {}}
	for _, up := range r.chain {
		if _, ok := seen[up.Name]; ok {
			continue
		}
		seen[up.Name] = struct{}{}
		out = append(out, up)
	}
	return out
}

func (r *resumingReader) fetchFrom(up Upstream, from int64, remainingAfter int) (io.ReadCloser, artifact.UpstreamMeta, error) {
	ropts := r.ropts
	if from > 0 {
		ropts.rangeStart = from
		ropts.hasRange = true
	} else {
		ropts.hasRange = false
		ropts.rangeStart = 0
	}
	// Same budget as Fetch: soft retry on non-final, full/compressed on last hop.
	budget := r.client.attemptBudget(nil, remainingAfter, up, remainingAfter == 0 && len(r.chain) > 1)
	body, meta, _, _, err := r.client.tryFetch(r.ctx, r.ref, up, nil, ropts, budget, remainingAfter)
	if err != nil {
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("upstream %s: resume at offset %d: %w", up.Name, from, err)
	}
	return body, meta, nil
}

func (r *resumingReader) Close() error {
	r.closed = true
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

func isResumableReadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errIdleBodyTimeout) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"unexpected eof",
		"i/o timeout",
		"idle body timeout",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	return false
}

// idleReadCloser wraps rc and fails a Read that blocks longer than idle with
// errIdleBodyTimeout so the resumingReader can Range-resume.
type idleReadCloser struct {
	rc   io.ReadCloser
	idle time.Duration
}

func newIdleReadCloser(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if rc == nil || idle <= 0 {
		return rc
	}
	return &idleReadCloser{rc: rc, idle: idle}
}

func (i *idleReadCloser) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := i.rc.Read(buf)
		ch <- result{n, err}
	}()
	timer := time.NewTimer(i.idle)
	defer timer.Stop()
	select {
	case r := <-ch:
		copy(p, buf[:r.n])
		return r.n, r.err
	case <-timer.C:
		_ = i.rc.Close()
		return 0, errIdleBodyTimeout
	}
}

func (i *idleReadCloser) Close() error {
	return i.rc.Close()
}

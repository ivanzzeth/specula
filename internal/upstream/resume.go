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

	// defaultIdleBodyTimeout is how long a body Read may block with no bytes
	// before we close the connection and attempt a Range resume.
	defaultIdleBodyTimeout = 60 * time.Second

	// defaultMaxResumeAttempts caps mid-stream Range retries per Fetch.
	defaultMaxResumeAttempts = 16

	defaultDialTimeout         = 30 * time.Second
	defaultTLSHandshakeTimeout = 30 * time.Second
)

// newUpstreamHTTPClient builds the shared HTTP client for upstream fetches.
// Client.Timeout is intentionally unset: an absolute deadline that includes the
// response body is what killed multi-minute OCI layer pulls (Client.Timeout
// while reading body → quarantine → 502). Safety nets live on the Transport
// (dial / TLS / response headers) and on the idle body reader instead.
func newUpstreamHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   defaultDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
			ResponseHeaderTimeout: defaultResponseHeaderTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// resumingReader wraps an upstream response body and transparently Range-resumes
// on mid-stream transport / idle failures against the same pinned upstream.
type resumingReader struct {
	client *fallbackClient
	ctx    context.Context
	ref    artifact.ArtifactRef
	up     Upstream
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
	return &resumingReader{
		client:      c,
		ctx:         ctx,
		ref:         ref,
		up:          up,
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
	// offset==0: connection died before any byte — retry full GET.
	// offset>0: Range resume from the byte already delivered to the caller.
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

	ropts := r.ropts
	from := r.offset
	if from > 0 {
		ropts.rangeStart = from
		ropts.hasRange = true
	} else {
		ropts.hasRange = false
		ropts.rangeStart = 0
	}

	body, meta, _, _, err := r.client.tryFetch(r.ctx, r.ref, r.up, nil, ropts, r.client.maxAttempts)
	if err != nil {
		return fmt.Errorf("upstream %s: resume at offset %d: %w", r.up.Name, from, err)
	}

	switch {
	case meta.StatusCode == http.StatusPartialContent && from > 0:
		r.body = newIdleReadCloser(body, r.idleTimeout)
		return nil
	case meta.StatusCode == http.StatusOK && from > 0:
		// Upstream ignored Range and resent the full object.
		r.body = newIdleReadCloser(body, r.idleTimeout)
		r.announceRestart = true
		return nil
	case meta.StatusCode >= 200 && meta.StatusCode < 300 && from == 0:
		r.body = newIdleReadCloser(body, r.idleTimeout)
		return nil
	default:
		_ = body.Close()
		return fmt.Errorf("upstream %s: resume unexpected status %d at offset %d",
			r.up.Name, meta.StatusCode, from)
	}
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
	// Bare EOF from a mid-stream close after some bytes is resumable; true
	// end-of-object EOF is handled before isResumableReadError is consulted
	// only when the server cleanly finished — hijack/close often surfaces as
	// unexpected EOF or "connection reset", but some stacks yield plain EOF
	// with a short body. Treat EOF as resumable when offset>0 (caller check).
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
	// Read into a private buffer so a timed-out goroutine cannot race on p.
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

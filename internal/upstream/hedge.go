package upstream

import (
	"context"
	"io"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// defaultHedgeDelay is how long the primary mutable fetch may run before a
// parallel attempt starts against the next mirror. Short enough to cut CN
// CDN hangs on tag/manifest GETs; long enough that a healthy primary usually
// wins without wasting the hedge.
const defaultHedgeDelay = 100 * time.Millisecond

// hedgeEligible reports whether Fetch should race the current upstream against
// the next for a Mutable (tag/index) request. Blobs stay sequential — racing
// multi-GB bodies wastes bandwidth.
func hedgeEligible(
	ref artifact.ArtifactRef,
	remainingAfter int,
	chain []Upstream,
	i int,
	blocker *blockTracker,
) bool {
	if !ref.Mutable || remainingAfter <= 0 || i+1 >= len(chain) {
		return false
	}
	next := chain[i+1]
	if blocker != nil && blocker.isBlocked(next.Name) {
		return false
	}
	return true
}

type hedgeOutcome struct {
	body      io.ReadCloser
	meta      artifact.UpstreamMeta
	latency   time.Duration
	transient bool
	err       error
	up        Upstream
}

// tryFetchHedged races primary against next after defaultHedgeDelay. The first
// successful headers win; the loser body (if any) is closed. On dual failure
// returns the primary's error (caller continues the chain).
func (c *fallbackClient) tryFetchHedged(
	ctx context.Context,
	ref artifact.ArtifactRef,
	primary, next Upstream,
	prev *artifact.UpstreamMeta,
	opts requestOpts,
	primaryBudget int,
	remainingAfter int,
) (io.ReadCloser, artifact.UpstreamMeta, time.Duration, bool, error, Upstream) {
	ctxP, cancelP := context.WithCancel(ctx)
	ctxN, cancelN := context.WithCancel(ctx)
	defer cancelP()
	defer cancelN()

	ch := make(chan hedgeOutcome, 2)

	go func() {
		body, meta, lat, transient, err := c.tryFetch(
			ctxP, ref, primary, prev, opts, primaryBudget, remainingAfter,
		)
		ch <- hedgeOutcome{body, meta, lat, transient, err, primary}
	}()

	go func() {
		timer := time.NewTimer(defaultHedgeDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			ch <- hedgeOutcome{err: ctx.Err(), up: next}
			return
		case <-timer.C:
		}
		body, meta, lat, transient, err := c.tryFetch(
			ctxN, ref, next, prev, opts, 1, 1,
		)
		ch <- hedgeOutcome{body, meta, lat, transient, err, next}
	}()

	var primaryOut *hedgeOutcome
	received := 0
	for received < 2 {
		select {
		case <-ctx.Done():
			return nil, artifact.UpstreamMeta{}, 0, false, ctx.Err(), primary
		case result := <-ch:
			received++
			copied := result
			if copied.up.Name == primary.Name {
				primaryOut = &copied
			}
			if copied.err != nil {
				continue
			}
			if copied.up.Name == primary.Name {
				cancelN()
			} else {
				cancelP()
			}
			// Drain the other attempt so a late success cannot leak a body.
			go func() {
				other := <-ch
				if other.body != nil {
					_ = other.body.Close()
				}
			}()
			return copied.body, copied.meta, copied.latency, false, nil, copied.up
		}
	}

	if primaryOut == nil {
		return nil, artifact.UpstreamMeta{}, 0, false, context.Canceled, primary
	}
	return nil, artifact.UpstreamMeta{}, 0, primaryOut.transient, primaryOut.err, primary
}

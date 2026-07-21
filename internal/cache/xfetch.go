package cache

import (
	"math"
	"math/rand"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// xfetchBeta is the XFetch aggressiveness parameter (paper default = 1).
const xfetchBeta = 1.0

// xfetchShouldRefresh reports whether this request should treat a still-hard-
// fresh mutable entry as expired (Lookup miss → revalidate) per the XFetch
// probabilistic early-expiration algorithm (Vattani / Antirez):
//
//	now >= expiry + delta * beta * ln(U)     where U ~ Uniform(0,1)
//
// Equivalently: now + (-delta*beta*ln(U)) >= expiry. As expiry approaches, the
// probability a given request "wins" the refresh rises, spreading stampedes.
//
// delta is an estimate of recomputation time; when unknown we use 1% of TTL
// (minimum 100ms) so early windows stay proportional to the configured TTL.
func xfetchShouldRefresh(fetchedAt time.Time, ttlSec int64, now time.Time, u float64) bool {
	if ttlSec <= 0 {
		return true
	}
	expiry := fetchedAt.Add(time.Duration(ttlSec) * time.Second)
	if !now.Before(expiry) {
		return true // hard expired
	}
	if u <= 0 {
		u = 1e-9 // avoid -Inf from ln(0)
	}
	if u >= 1 {
		u = 1 - 1e-9
	}
	delta := xfetchDelta(ttlSec)
	// now >= expiry + delta*beta*ln(u)  (ln(u) is negative)
	threshold := expiry.Add(time.Duration(float64(delta) * xfetchBeta * math.Log(u)))
	return !now.Before(threshold)
}

func xfetchDelta(ttlSec int64) time.Duration {
	d := time.Duration(ttlSec) * time.Second / 100 // 1% of TTL
	const min = 100 * time.Millisecond
	if d < min {
		return min
	}
	return d
}

// xfetchUniform draws U ~ Uniform(0,1) for XFetch; tests replace it.
var xfetchUniform = rand.Float64

// isMutableFresh reports whether the mutable entry's TTL window has not
// expired. Sentinels: -1 = never expire, 0 = always expired. Positive TTLs
// apply XFetch soft-expiry so a fraction of requests revalidate before the
// hard cliff (ARCHITECTURE: probabilistic early refresh).
func isMutableFresh(e *artifact.MutableEntry) bool {
	return isMutableFreshAt(e, time.Now(), xfetchUniform())
}

func isMutableFreshAt(e *artifact.MutableEntry, now time.Time, u float64) bool {
	switch e.TTLSeconds {
	case ttlNeverRevalidate:
		return true
	case ttlAlwaysRevalidate:
		return false
	default:
		if xfetchShouldRefresh(e.FetchedAt, e.TTLSeconds, now, u) {
			return false
		}
		return true
	}
}

// isHardExpired reports whether the mutable entry is past its absolute TTL
// cliff (no XFetch). Soft-expired-but-not-hard entries are still served via
// Lookup with SoftExpired=true (stale-while-revalidate).
func isHardExpired(e *artifact.MutableEntry) bool {
	return isHardExpiredAt(e, time.Now())
}

func isHardExpiredAt(e *artifact.MutableEntry, now time.Time) bool {
	switch e.TTLSeconds {
	case ttlNeverRevalidate:
		return false
	case ttlAlwaysRevalidate:
		return true
	default:
		expiry := e.FetchedAt.Add(time.Duration(e.TTLSeconds) * time.Second)
		return !now.Before(expiry)
	}
}

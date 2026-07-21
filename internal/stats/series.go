package stats

import "sync"

// DefaultSeriesCapacity is how many samples the in-memory ring retains per
// series key (~1h at the default 30s refresh interval).
const DefaultSeriesCapacity = 120

// SeriesPoint is one capacity sample (Unix seconds + byte total).
type SeriesPoint struct {
	Unix  int64
	Bytes int64
}

// seriesRing is a fixed-capacity circular buffer of SeriesPoint values.
// Not safe for concurrent use without an external lock.
type seriesRing struct {
	buf  []SeriesPoint
	cap  int
	head int // next write index
	len  int
}

func newSeriesRing(capacity int) *seriesRing {
	if capacity <= 0 {
		capacity = DefaultSeriesCapacity
	}
	return &seriesRing{buf: make([]SeriesPoint, capacity), cap: capacity}
}

func (r *seriesRing) push(p SeriesPoint) {
	r.buf[r.head] = p
	r.head = (r.head + 1) % r.cap
	if r.len < r.cap {
		r.len++
	}
}

// snapshot returns oldest→newest points.
func (r *seriesRing) snapshot() []SeriesPoint {
	if r.len == 0 {
		return nil
	}
	out := make([]SeriesPoint, r.len)
	start := 0
	if r.len == r.cap {
		start = r.head
	}
	for i := 0; i < r.len; i++ {
		out[i] = r.buf[(start+i)%r.cap]
	}
	return out
}

// seriesStore holds per-protocol rings plus the "" key for the grand total.
type seriesStore struct {
	mu   sync.Mutex
	cap  int
	byKey map[string]*seriesRing
}

func newSeriesStore(capacity int) *seriesStore {
	if capacity <= 0 {
		capacity = DefaultSeriesCapacity
	}
	return &seriesStore{cap: capacity, byKey: map[string]*seriesRing{}}
}

func (s *seriesStore) ring(key string) *seriesRing {
	r, ok := s.byKey[key]
	if !ok {
		r = newSeriesRing(s.cap)
		s.byKey[key] = r
	}
	return r
}

func (s *seriesStore) record(proto string, bytes int64, unix int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring(proto).push(SeriesPoint{Unix: unix, Bytes: bytes})
}

func (s *seriesStore) snapshot(proto string) []SeriesPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byKey[proto]
	if !ok {
		return nil
	}
	return r.snapshot()
}

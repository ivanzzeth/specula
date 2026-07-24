// Package events is an in-process ring buffer of verification outcomes for the
// admin Events UI (GET /api/v1/admin/events).
//
// Fail and warn results from the cache verify-on-write path are appended here.
// The store is intentionally process-local (not replicated across HA replicas):
// it is an operator live feed, not a durable audit log. Capacity is bounded.
package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

const DefaultCapacity = 500

// Event is one verification outcome suitable for the admin feed.
type Event struct {
	ID       int64  `json:"id"`
	Unix     int64  `json:"unix"`
	Protocol string `json:"protocol"`
	Artifact string `json:"artifact"`
	Digest   string `json:"digest"`
	Tier     string `json:"tier"`
	Result   string `json:"result"` // "pass" | "fail" | "warn"
	Detail   string `json:"detail"`
}

// Store appends and lists recent verification events.
type Store interface {
	Record(ctx context.Context, e Event)
	List(ctx context.Context, limit int) []Event
}

// Memory is a bounded ring buffer Store.
type Memory struct {
	cap int
	mu  sync.Mutex
	buf []Event
	seq atomic.Int64
}

// NewMemory constructs a ring buffer with the given capacity (clamped ≥ 1).
func NewMemory(capacity int) *Memory {
	if capacity < 1 {
		capacity = DefaultCapacity
	}
	return &Memory{cap: capacity, buf: make([]Event, 0, capacity)}
}

// Record appends e, assigning ID and Unix when zero.
func (m *Memory) Record(_ context.Context, e Event) {
	if e.Unix == 0 {
		e.Unix = time.Now().UTC().Unix()
	}
	if e.ID == 0 {
		e.ID = m.seq.Add(1)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) >= m.cap {
		// Drop oldest.
		copy(m.buf, m.buf[1:])
		m.buf[len(m.buf)-1] = e
		return
	}
	m.buf = append(m.buf, e)
}

// List returns the newest events first, up to limit (default 100, max = cap).
func (m *Memory) List(_ context.Context, limit int) []Event {
	if limit <= 0 {
		limit = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > len(m.buf) {
		limit = len(m.buf)
	}
	out := make([]Event, limit)
	for i := 0; i < limit; i++ {
		out[i] = m.buf[len(m.buf)-1-i]
	}
	return out
}

// FromVerify builds an Event from a cache verify outcome.
func FromVerify(ref artifact.ArtifactRef, digest string, res artifact.Result) Event {
	result := "pass"
	switch res.Status {
	case artifact.StatusFail:
		result = "fail"
	case artifact.StatusWarn:
		result = "warn"
	}
	art := ref.Name
	if ref.Version != "" {
		art = ref.Name + ":" + ref.Version
	}
	return Event{
		Protocol: ref.Protocol,
		Artifact: art,
		Digest:   digest,
		Tier:     res.Tier.String(),
		Result:   result,
		Detail:   res.Message,
	}
}

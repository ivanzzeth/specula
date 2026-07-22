// Package runtimestate persists HA-shared runtime state (stats time series and
// upstream auto-block circuit breaker) in the metadata database.
package runtimestate

import (
	"context"
	"time"

	"github.com/ivanzzeth/specula/internal/stats"
)

// BlockState is persisted auto-block circuit breaker state for one upstream mirror.
type BlockState struct {
	Failures     int
	BlockedUntil time.Time // zero when not blocked
}

// SeriesStore records and reads capacity time-series samples. Protocol "" is the
// grand total across all protocols.
type SeriesStore interface {
	Record(ctx context.Context, protocol string, bytes int64, unix int64) error
	Snapshot(ctx context.Context, protocol string, limit int) ([]stats.SeriesPoint, error)
}

// BlockStore persists upstream auto-block state keyed by (protocol, upstream).
type BlockStore interface {
	Get(ctx context.Context, protocol, upstream string) (BlockState, error)
	Set(ctx context.Context, protocol, upstream string, state BlockState) error
	Clear(ctx context.Context, protocol, upstream string) error
}

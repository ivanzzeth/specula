// Package coalesce re-exports stampede-protection primitives.
package coalesce

import (
	"time"

	intcoalesce "github.com/ivanzzeth/specula/internal/coalesce"
)

type (
	Coalescer  = intcoalesce.Coalescer
	Locker     = intcoalesce.Locker
	Lock       = intcoalesce.Lock
	Result     = intcoalesce.Result
	PanicError = intcoalesce.PanicError
)

// NewLocalCoalescer returns an in-process sharded singleflight Coalescer.
func NewLocalCoalescer() Coalescer {
	return intcoalesce.NewLocalCoalescer()
}

// NewLocalLocker returns an in-process Locker with TTL + fenced release.
func NewLocalLocker() Locker {
	return intcoalesce.NewLocalLocker()
}

// DefaultLockTTL is a reasonable default for Locker.Acquire.
const DefaultLockTTL = 30 * time.Second

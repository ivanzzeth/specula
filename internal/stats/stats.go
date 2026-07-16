// Package stats aggregates per-protocol and total cache capacity from the
// authoritative MetadataStore (write-time size records, not FS walks — G7), with
// a du/statfs fallback (gopsutil) for opaque caches such as git bare mirrors,
// and exports Prometheus gauges for Grafana dashboarding.
package stats

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// Collector aggregates cache capacity statistics.
type Collector interface {
	// ByProtocol returns per-protocol size stats (SUM/COUNT/oldest/newest).
	ByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error)
	// Total returns the grand-total size stat across all protocols.
	Total(ctx context.Context) (artifact.SizeStat, error)
	// RecordPut increments the aggregate for protocol by size bytes.
	RecordPut(ctx context.Context, protocol string, size int64) error
	// RecordEvict decrements the aggregate for protocol by size bytes.
	RecordEvict(ctx context.Context, protocol string, size int64) error
}

// inmemStat holds in-memory byte and object counters for standalone mode.
type inmemStat struct {
	bytes   int64
	objects int64
}

// collector is the concrete Collector implementation.
type collector struct {
	store        meta.MetadataStore // nil = standalone mode (Prometheus-gauge-only)
	cacheBytes   *prometheus.GaugeVec
	cacheObjects *prometheus.GaugeVec
	duPaths      []string // opaque-cache roots for du-sb fallback (e.g., git bare mirror)
	mu           sync.Mutex
	// inmem tracks per-protocol bytes/objects when store == nil.
	inmem map[string]inmemStat
}

// Compile-time assertion.
var _ Collector = (*collector)(nil)

// NewCollector constructs the default Collector backed by the default Prometheus
// registry, with no MetadataStore (standalone / Prometheus-gauge-only mode).
// ByProtocol and Total reflect in-memory counters kept by RecordPut/RecordEvict.
func NewCollector() Collector {
	return newCollector(nil, prometheus.DefaultRegisterer)
}

// NewCollectorWithStore constructs a Collector that reads authoritative
// per-protocol and total cache sizes from the MetadataStore (O(1) SUM GROUP BY)
// and keeps Prometheus gauges in sync on every ByProtocol call.
func NewCollectorWithStore(store meta.MetadataStore) Collector {
	return newCollector(store, prometheus.DefaultRegisterer)
}

// newCollector is the internal constructor that accepts a Registerer so tests
// can inject a fresh prometheus.NewRegistry() and avoid duplicate-registration
// panics across sub-tests.
func newCollector(store meta.MetadataStore, reg prometheus.Registerer) *collector {
	bytesVec := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "specula_cache_bytes",
			Help: "Cached bytes per protocol (use sum() in Grafana for the total).",
		},
		[]string{"protocol"},
	)
	objectsVec := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "specula_cache_objects",
			Help: "Cached object count per protocol.",
		},
		[]string{"protocol"},
	)
	bytesVec = registerOrExisting(reg, bytesVec)
	objectsVec = registerOrExisting(reg, objectsVec)

	return &collector{
		store:        store,
		cacheBytes:   bytesVec,
		cacheObjects: objectsVec,
		inmem:        make(map[string]inmemStat),
	}
}

// registerOrExisting registers c with reg and returns it. On
// AlreadyRegisteredError it returns the previously registered *GaugeVec. Any
// other registration error causes a panic (programmer error, bad metric def).
func registerOrExisting(reg prometheus.Registerer, c *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			existing, ok := are.ExistingCollector.(*prometheus.GaugeVec)
			if !ok {
				panic(fmt.Sprintf("stats: prometheus metric already registered as wrong type: %T", are.ExistingCollector))
			}
			return existing
		}
		panic(fmt.Sprintf("stats: prometheus registration failed: %v", err))
	}
	return c
}

// ByProtocol returns per-protocol SizeStat.
//
// When a MetadataStore is available it calls CacheSizeByProtocol (a single
// O(1) SUM GROUP BY query) and re-syncs the Prometheus gauges. In standalone
// mode it returns the in-memory counters maintained by RecordPut/RecordEvict.
func (c *collector) ByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error) {
	if c.store != nil {
		stats, err := c.store.CacheSizeByProtocol(ctx)
		if err != nil {
			return nil, fmt.Errorf("stats: CacheSizeByProtocol: %w", err)
		}
		// Re-sync Prometheus gauges from the authoritative store on each call so
		// that out-of-band GC or multi-node writes are reflected promptly.
		for proto, s := range stats {
			c.cacheBytes.WithLabelValues(proto).Set(float64(s.Bytes))
			c.cacheObjects.WithLabelValues(proto).Set(float64(s.Objects))
		}
		return stats, nil
	}

	// Standalone mode: snapshot the in-memory counters.
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]artifact.SizeStat, len(c.inmem))
	for proto, s := range c.inmem {
		result[proto] = artifact.SizeStat{
			Bytes:   s.bytes,
			Objects: s.objects,
		}
	}
	return result, nil
}

// Total returns the grand-total SizeStat across all protocols. It sums
// ByProtocol; for opaque cache directories registered via AddDUPath (e.g., git
// bare mirror roots), it adds a best-effort directory-walk byte count (du -sb).
func (c *collector) Total(ctx context.Context) (artifact.SizeStat, error) {
	byProto, err := c.ByProtocol(ctx)
	if err != nil {
		return artifact.SizeStat{}, err
	}

	var total artifact.SizeStat
	for _, s := range byProto {
		total.Bytes += s.Bytes
		total.Objects += s.Objects
		// Accumulate Oldest/Newest across all protocols, skipping zero times.
		if !s.Oldest.IsZero() && (total.Oldest.IsZero() || s.Oldest.Before(total.Oldest)) {
			total.Oldest = s.Oldest
		}
		if s.Newest.After(total.Newest) {
			total.Newest = s.Newest
		}
	}

	// Add du-sb byte counts for opaque cache roots.
	c.mu.Lock()
	paths := append([]string(nil), c.duPaths...) // snapshot under lock
	c.mu.Unlock()

	for _, path := range paths {
		size, err := duBytes(path)
		if err != nil {
			// best-effort: don't fail Total() for unreachable paths
			continue
		}
		total.Bytes += size
	}

	return total, nil
}

// RecordPut increments Prometheus gauges and in-memory counters for protocol
// by size bytes and one object. Called by the cache manager after a successful
// verify-on-write promotion into the CAS layer.
func (c *collector) RecordPut(ctx context.Context, protocol string, size int64) error {
	c.cacheBytes.WithLabelValues(protocol).Add(float64(size))
	c.cacheObjects.WithLabelValues(protocol).Add(1)

	c.mu.Lock()
	s := c.inmem[protocol]
	s.bytes += size
	s.objects++
	c.inmem[protocol] = s
	c.mu.Unlock()

	return nil
}

// RecordEvict decrements Prometheus gauges and in-memory counters for protocol
// by size bytes and one object. Counters are clamped to zero to guard against
// ordering races during GC. Called by the cache manager on eviction.
func (c *collector) RecordEvict(ctx context.Context, protocol string, size int64) error {
	c.cacheBytes.WithLabelValues(protocol).Sub(float64(size))
	c.cacheObjects.WithLabelValues(protocol).Sub(1)

	c.mu.Lock()
	s := c.inmem[protocol]
	s.bytes -= size
	if s.bytes < 0 {
		s.bytes = 0
	}
	s.objects--
	if s.objects < 0 {
		s.objects = 0
	}
	c.inmem[protocol] = s
	c.mu.Unlock()

	return nil
}

// AddDUPath registers an opaque-cache root for the du-sb fallback in Total().
// Paths are walked with filepath.WalkDir on each Total() call; failures are
// silently skipped (best-effort). Intended for git bare mirror repositories
// whose blobs are not tracked in the MetadataStore.
func (c *collector) AddDUPath(path string) {
	c.mu.Lock()
	c.duPaths = append(c.duPaths, path)
	c.mu.Unlock()
}

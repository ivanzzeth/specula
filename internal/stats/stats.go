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
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// CollectorConfig controls the background refresh loop behaviour.
type CollectorConfig struct {
	// RefreshInterval is how often Run re-reads MetadataStore.CacheSizeByProtocol
	// to re-sync the Prometheus gauges. Zero or negative values default to 30s.
	RefreshInterval time.Duration

	// EnableDUFallback, when true, causes the background refresh loop to also
	// recompute disk usage for opaque-cache roots registered via AddOpaquePath
	// and update the corresponding Prometheus gauge specula_cache_bytes{protocol}.
	// AddOpaquePath roots are always included in Total() regardless of this flag.
	EnableDUFallback bool
}

// DefaultCollectorConfig returns production-safe defaults (30 s interval, du
// fallback disabled).
func DefaultCollectorConfig() CollectorConfig {
	return CollectorConfig{
		RefreshInterval:  30 * time.Second,
		EnableDUFallback: false,
	}
}

// Collector aggregates cache capacity statistics.
type Collector interface {
	// ByProtocol returns per-protocol size stats (SUM/COUNT/oldest/newest).
	// Opaque-cache roots registered via AddOpaquePath are always included.
	ByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error)
	// Total returns the grand-total size stat across all protocols.
	Total(ctx context.Context) (artifact.SizeStat, error)
	// RecordPut increments the aggregate for protocol by size bytes.
	RecordPut(ctx context.Context, protocol string, size int64) error
	// RecordEvict decrements the aggregate for protocol by size bytes.
	RecordEvict(ctx context.Context, protocol string, size int64) error
	// Run blocks, ticking every RefreshInterval, re-reading the MetadataStore
	// and updating Prometheus gauges. Returns when ctx is cancelled. Call it in
	// a dedicated goroutine:
	//
	//	go collector.Run(ctx)
	//
	// In standalone mode (no MetadataStore) it is a context-aware no-op loop.
	Run(ctx context.Context)
	// AddOpaquePath registers an opaque-cache root (e.g., a git bare-mirror
	// directory) with its Prometheus protocol label. The path is included in
	// ByProtocol, Total, and the background refresh loop via a du-sb directory
	// walk. Idempotent: duplicate (path, protocol) pairs are silently ignored.
	AddOpaquePath(path, protocol string)
}

// inmemStat holds in-memory byte and object counters for standalone mode.
type inmemStat struct {
	bytes   int64
	objects int64
}

// duEntry pairs an opaque-cache root directory with its Prometheus protocol
// label (e.g. {path: "/var/cache/specula/git", protocol: "git"}).
type duEntry struct {
	path     string
	protocol string
}

// collector is the concrete Collector implementation.
type collector struct {
	store        meta.MetadataStore // nil = standalone mode (Prometheus-gauge-only)
	cacheBytes   *prometheus.GaugeVec
	cacheObjects *prometheus.GaugeVec
	cfg          CollectorConfig
	mu           sync.Mutex
	// inmem tracks per-protocol bytes/objects when store == nil.
	inmem   map[string]inmemStat
	duPaths []duEntry // opaque-cache roots for du-sb fallback

	// tickCh, when non-nil, replaces time.NewTicker inside Run.
	// Intended only for unit tests that drive refresh ticks deterministically.
	tickCh <-chan time.Time
}

// Compile-time assertion.
var _ Collector = (*collector)(nil)

// NewCollector constructs the default Collector backed by the default Prometheus
// registry, with no MetadataStore (standalone / Prometheus-gauge-only mode).
// ByProtocol and Total reflect in-memory counters kept by RecordPut/RecordEvict.
func NewCollector() Collector {
	return newCollector(nil, prometheus.DefaultRegisterer, DefaultCollectorConfig())
}

// NewCollectorWithStore constructs a Collector that reads authoritative
// per-protocol and total cache sizes from the MetadataStore (O(1) SUM GROUP BY)
// and keeps Prometheus gauges in sync on every ByProtocol call.
func NewCollectorWithStore(store meta.MetadataStore) Collector {
	return newCollector(store, prometheus.DefaultRegisterer, DefaultCollectorConfig())
}

// NewCollectorWithConfig constructs a standalone Collector (no MetadataStore)
// with caller-supplied configuration.
func NewCollectorWithConfig(cfg CollectorConfig) Collector {
	return newCollector(nil, prometheus.DefaultRegisterer, cfg)
}

// NewCollectorWithStoreAndConfig constructs a store-backed Collector with
// caller-supplied configuration.
func NewCollectorWithStoreAndConfig(store meta.MetadataStore, cfg CollectorConfig) Collector {
	return newCollector(store, prometheus.DefaultRegisterer, cfg)
}

// newCollector is the internal constructor that accepts a Registerer so tests
// can inject a fresh prometheus.NewRegistry() and avoid duplicate-registration
// panics across sub-tests.
func newCollector(store meta.MetadataStore, reg prometheus.Registerer, cfg CollectorConfig) *collector {
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
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
		cfg:          cfg,
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

// Run blocks, ticking every RefreshInterval (or waiting on the injected tickCh
// in test mode), and calls refreshOnce on each tick. Returns when ctx is
// cancelled or when the tick channel is closed.
func (c *collector) Run(ctx context.Context) {
	var ch <-chan time.Time
	if c.tickCh != nil {
		// Test injection: use the provided channel instead of a real ticker.
		ch = c.tickCh
	} else {
		t := time.NewTicker(c.cfg.RefreshInterval)
		defer t.Stop()
		ch = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			c.refreshOnce(ctx)
		}
	}
}

// refreshOnce performs one refresh cycle: reads CacheSizeByProtocol from the
// MetadataStore, sets the Prometheus gauges to the authoritative values, and
// then always walks every opaque-cache root registered via AddOpaquePath to
// update their specula_cache_bytes{protocol} gauge.
//
// The EnableDUFallback flag is kept for API compatibility but no longer gates
// already-registered paths: any path explicitly registered via AddOpaquePath is
// unconditionally walked on every refresh. This is the fix for git being
// invisible in /metrics (the flag's original early-return guard silently
// prevented the git gauge from ever being set).
//
// In standalone mode (store == nil) it is a no-op because RecordPut/RecordEvict
// already keep the gauges current between refreshes.
//
// Prometheus GaugeVec Set/Add operations are goroutine-safe, so refreshOnce is
// safe to call concurrently with RecordPut and RecordEvict. The duPaths slice is
// snapshotted under c.mu to avoid holding the lock during potentially slow FS
// walks.
func (c *collector) refreshOnce(ctx context.Context) {
	if c.store == nil {
		// Standalone mode: gauges are kept current by RecordPut/RecordEvict.
		return
	}

	stats, err := c.store.CacheSizeByProtocol(ctx)
	if err != nil {
		// Best-effort: don't crash the refresh loop on transient DB errors.
		return
	}
	for proto, s := range stats {
		c.cacheBytes.WithLabelValues(proto).Set(float64(s.Bytes))
		c.cacheObjects.WithLabelValues(proto).Set(float64(s.Objects))
	}

	// Always walk opaque-cache roots registered via AddOpaquePath (e.g., git
	// bare mirror dirs). This unconditional walk replaces the former
	// EnableDUFallback gate which silently prevented the git Prometheus gauge
	// from being set, causing git to be invisible in /metrics even when it was
	// caching correctly.
	c.mu.Lock()
	paths := append([]duEntry(nil), c.duPaths...)
	c.mu.Unlock()

	if len(paths) == 0 {
		return
	}

	// Accumulate per-protocol byte totals across all registered paths.
	byProto := make(map[string]int64, len(paths))
	for _, e := range paths {
		size, err := duBytes(e.path)
		if err != nil {
			continue // best-effort: skip unreachable dirs silently
		}
		byProto[e.protocol] += size
	}
	for proto, size := range byProto {
		c.cacheBytes.WithLabelValues(proto).Set(float64(size))
	}
}

// ByProtocol returns per-protocol SizeStat.
//
// When a MetadataStore is available it calls CacheSizeByProtocol (a single
// O(1) SUM GROUP BY query) and re-syncs the Prometheus gauges. In standalone
// mode it returns the in-memory counters maintained by RecordPut/RecordEvict.
//
// In both modes, opaque-cache roots registered via AddOpaquePath (e.g., git
// bare mirror directories) are always included: their byte totals are computed
// via a du-sb directory walk and merged into the result map. This is the fix
// for git being invisible in GET /api/v1/admin/stats and /metrics — git objects
// live on disk as bare mirrors, not in the CAS / MetadataStore, so they were
// silently absent from ByProtocol before this change.
func (c *collector) ByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error) {
	var result map[string]artifact.SizeStat

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
		// Copy the map so we can safely add opaque-path bytes below without
		// mutating the store's internal representation.
		result = make(map[string]artifact.SizeStat, len(stats))
		for k, v := range stats {
			// CAS/metadata rows are counted exactly by the SUM/COUNT query.
			v.ObjectsCountable = true
			result[k] = v
		}
	} else {
		// Standalone mode: snapshot the in-memory counters.
		c.mu.Lock()
		result = make(map[string]artifact.SizeStat, len(c.inmem))
		for proto, s := range c.inmem {
			result[proto] = artifact.SizeStat{
				Bytes:            s.bytes,
				Objects:          s.objects,
				ObjectsCountable: true, // RecordPut/RecordEvict maintain an exact count
			}
		}
		c.mu.Unlock()
	}

	// Always merge in du-computed bytes for registered opaque-cache paths.
	// Snapshot the slice under lock so a concurrent AddOpaquePath does not race.
	c.mu.Lock()
	paths := append([]duEntry(nil), c.duPaths...)
	c.mu.Unlock()

	if len(paths) == 0 {
		return result, nil
	}

	// Accumulate per-protocol byte totals across all registered opaque paths.
	byProto := make(map[string]int64, len(paths))
	for _, e := range paths {
		size, err := duBytes(e.path)
		if err != nil {
			continue // best-effort: skip unreachable dirs silently
		}
		byProto[e.protocol] += size
	}
	for proto, size := range byProto {
		s := result[proto]
		s.Bytes += size
		// Opaque caches contribute bytes but no countable objects: their content
		// lives inside packfiles/blobs on disk, not as CAS rows. Mark the object
		// count UNKNOWN rather than letting a 0 masquerade as "counted, none".
		// If a protocol mixes CAS rows with an opaque root, the count is only
		// partial, which is likewise not a trustworthy number.
		s.Objects = 0
		s.ObjectsCountable = false
		result[proto] = s
		// Also update the Prometheus gauge so /metrics reflects the opaque bytes
		// immediately without waiting for the next background-refresh tick.
		c.cacheBytes.WithLabelValues(proto).Set(float64(result[proto].Bytes))
	}

	return result, nil
}

// Total returns the grand-total SizeStat across all protocols. It sums
// ByProtocol, which already includes du-computed bytes for any opaque-cache
// roots registered via AddOpaquePath (e.g., git bare mirror directories).
// There is no separate du loop here; ByProtocol is the single source of truth
// for opaque-path bytes to avoid double-counting.
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

	return total, nil
}

// RecordPut increments Prometheus gauges and in-memory counters for protocol
// by size bytes and one object. Called by the cache manager after a successful
// verify-on-write promotion into the CAS layer.
//
// Gauge updates are atomic from Prometheus's perspective and happen before the
// next background refresh overwrites them with the authoritative store values;
// this keeps gauges accurate between refresh ticks.
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
//
// Gauge updates happen before the next background refresh; any drift introduced
// by concurrent GC is corrected at the next refresh tick.
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

// AddOpaquePath registers an opaque-cache root paired with its Prometheus
// protocol label for the du-sb fallback in ByProtocol(), Total(), and the
// background refresh loop. The protocol argument becomes the label value on
// specula_cache_bytes{protocol=...}. Failures are silently skipped (best-effort).
// Intended for git bare mirror repositories whose blobs are not tracked in the
// MetadataStore.
//
// Idempotent: duplicate (path, protocol) pairs are silently ignored so callers
// can safely register on every request without inflating byte counts.
func (c *collector) AddOpaquePath(path, protocol string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.duPaths {
		if e.path == path && e.protocol == protocol {
			return // already registered
		}
	}
	c.duPaths = append(c.duPaths, duEntry{path: path, protocol: protocol})
}

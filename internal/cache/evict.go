package cache

import (
	"context"
	"fmt"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// evictBatch is how many unpinned candidates we pull per ListEntries round.
const evictBatch = 64

// enforceCapacity evicts the oldest unpinned immutable entries until total
// cached bytes are <= maxBytes. protect is never evicted (the entry just
// stored). No-op when maxBytes <= 0.
//
// Eviction deletes the metadata row and best-effort deletes the CAS blob so
// disk is actually freed. Shared digests across remaining entries may cause a
// temporary "meta hit, blob miss" that Lookup already treats as a miss.
func (m *manager) enforceCapacity(ctx context.Context, protect artifact.ArtifactRef) error {
	if m.maxBytes <= 0 {
		return nil
	}

	m.evictMu.Lock()
	defer m.evictMu.Unlock()

	total, err := m.totalCachedBytes(ctx)
	if err != nil {
		return err
	}
	if total <= m.maxBytes {
		return nil
	}

	need := total - m.maxBytes
	m.log.Info("cache: over capacity, evicting",
		"total_bytes", total, "max_bytes", m.maxBytes, "need_bytes", need)

	unpinned := false
	cachedOnly := artifact.OriginCached
	evicted := 0
	var freed int64

	for need > 0 {
		page, listErr := m.meta.ListEntries(ctx, "", meta.EntryFilter{
			Pinned: &unpinned,
			Origin: cachedOnly,
		}, meta.Page{
			Limit:  evictBatch,
			Sort:   meta.SortCreatedAt,
			Desc:   false, // oldest first
			Offset: 0,
		})
		if listErr != nil {
			return fmt.Errorf("list eviction candidates: %w", listErr)
		}
		if len(page.Entries) == 0 {
			m.log.Warn("cache: still over capacity but no unpinned candidates",
				"total_bytes", total-freed, "max_bytes", m.maxBytes, "evicted", evicted)
			break
		}

		progress := false
		for _, e := range page.Entries {
			if need <= 0 {
				break
			}
			if sameRef(e.Ref, protect) {
				continue
			}
			size := e.Size
			digest := e.Digest
			proto := e.Protocol

			if delErr := m.meta.Delete(ctx, e.Ref); delErr != nil {
				return fmt.Errorf("evict meta %s/%s@%s: %w",
					e.Ref.Protocol, e.Ref.Name, e.Ref.Version, delErr)
			}
			if digest != "" {
				if blobErr := m.blobs.Delete(ctx, digest); blobErr != nil {
					m.log.Warn("cache: evict blob", "digest", digest, "err", blobErr)
				}
			}
			if m.onEvict != nil {
				m.onEvict(ctx, proto, size)
			}
			need -= size
			freed += size
			evicted++
			progress = true
		}
		if !progress {
			// Only the protected entry (or pinned-filtered empties) remained.
			m.log.Warn("cache: still over capacity; protected/pinned entries block further eviction",
				"max_bytes", m.maxBytes, "evicted", evicted)
			break
		}
	}

	if evicted > 0 {
		m.log.Info("cache: capacity eviction complete",
			"evicted", evicted, "freed_bytes", freed, "max_bytes", m.maxBytes)
	}
	return nil
}

func (m *manager) totalCachedBytes(ctx context.Context) (int64, error) {
	// Capacity pressure applies only to pull-through cache. Hosted content is
	// authoritative and never counted toward the eviction budget.
	if sizer, ok := m.meta.(interface {
		CacheSizeByOrigin(context.Context) (map[string]artifact.SizeStat, error)
	}); ok {
		byOrigin, err := sizer.CacheSizeByOrigin(ctx)
		if err != nil {
			return 0, fmt.Errorf("cache size by origin: %w", err)
		}
		return byOrigin[artifact.OriginCached].Bytes, nil
	}
	stats, err := m.meta.CacheSizeByProtocol(ctx)
	if err != nil {
		return 0, fmt.Errorf("cache size: %w", err)
	}
	var total int64
	for _, s := range stats {
		total += s.Bytes
	}
	return total, nil
}

func sameRef(a, b artifact.ArtifactRef) bool {
	return a.Protocol == b.Protocol && a.Name == b.Name && a.Version == b.Version
}

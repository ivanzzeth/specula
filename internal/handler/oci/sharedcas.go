package oci

import (
	"context"
	"fmt"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// Cross-name reuse of blobs already in the CAS.
//
// Blob storage is content-addressed and name-independent: the key is
// blobs/<algo>/<hex>, so identical bytes are ONE object however many repositories
// reference them. The cache LOOKUP, though, keys on (protocol, name, digest) —
// necessarily, because a hosted private repo's blob must not be readable by
// asking for it under a name the caller happens to have access to.
//
// The consequence was that a pull of library/redis missed on bytes Specula
// physically had, because they had arrived under docker.io/library/redis or under
// some other repository sharing the layer. Specula would then re-download the
// whole layer from upstream and discover at Put time that the object already
// existed, discarding it. Storage was never wasted; bandwidth and latency were,
// and — the part that actually broke a cluster — when every upstream is failing,
// a blob we already hold became a 502.
//
// So: on a miss for (name, digest), look for the same digest under ANY name whose
// content is public pull-through (origin=cached), and serve those bytes.
//
// Why origin=cached is the whole safety argument: pull-through content came from
// public upstreams, so serving it under a different public name discloses nothing
// that was not already public. Hosted content (origin=hosted) is excluded — it may
// be a private org's push, and blob digests are readable from a manifest, so a
// digest-keyed shortcut would otherwise be a private-content read primitive.
// Callers must also skip this for hosted/owned names, which are authoritative
// local content where a miss is a definitive 404.
//
// INVARIANT worth stating because a future change could break it silently: this
// is only safe while every configured upstream is anonymous/public. If Specula
// ever grows per-upstream credentials, content fetched from an authenticated
// private mirror must stop being marked origin=cached, or gain an explicit
// "public" flag, before this lookup can keep serving it across names.

// findSharedBlob returns an existing cache entry for digest recorded under some
// OTHER repository name, provided that entry is public pull-through content.
// Returns nil when there is none, or when no metadata store is wired.
func (h *Handler) findSharedBlob(ctx context.Context, imageName, digest string) *artifact.CacheEntry {
	if h.meta == nil || digest == "" {
		return nil
	}
	page, err := h.meta.ListEntries(ctx, "oci", meta.EntryFilter{
		Digest: digest,
		Origin: artifact.OriginCached,
	}, meta.Page{Limit: 4})
	if err != nil {
		// Not fatal: the caller falls through to upstream exactly as before.
		h.log.Warn("oci: shared-CAS lookup failed", "digest", digest, "err", err)
		return nil
	}
	for _, e := range page.Entries {
		if e.Ref.Name == imageName {
			continue // the direct lookup already missed on this one
		}
		if artifact.NormalizeOrigin(e.Origin) != artifact.OriginCached {
			continue
		}
		if e.Digest != digest {
			continue
		}
		entry := e.CacheEntry
		return &entry
	}
	return nil
}

// adoptSharedBlob records the shared digest under imageName so subsequent pulls
// are a direct hit instead of repeating the cross-name search.
//
// This adds a metadata ROW, not bytes: the CAS object is the same one. It also
// keeps the tier and upstream of the original entry, because those describe how
// these exact bytes were verified — inventing a fresh "unverified" row for
// content that passed the chain once would understate what is known about it.
func (h *Handler) adoptSharedBlob(ctx context.Context, imageName string, src *artifact.CacheEntry) {
	if h.meta == nil || src == nil {
		return
	}
	adopted := *src
	adopted.Ref = artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  src.Digest,
		Digest:   src.Digest,
		Mutable:  false,
	}
	adopted.Origin = artifact.OriginCached
	if err := h.meta.Put(ctx, adopted); err != nil {
		// Purely an optimisation for next time; the request itself already has
		// its bytes.
		h.log.Warn("oci: could not record shared blob under this name",
			"name", imageName, "digest", src.Digest, "err", err)
		return
	}
	h.log.Debug("oci: reused a CAS blob across names",
		"name", imageName, "from", src.Ref.Name, "digest", src.Digest)
}

// sharedBlobEntryFor returns the entry to serve when digest is already in the CAS
// under another public name, or nil to continue to upstream. It performs the
// adoption as a side effect so the next request skips this path.
func (h *Handler) sharedBlobEntryFor(ctx context.Context, imageName, digest string) *artifact.CacheEntry {
	src := h.findSharedBlob(ctx, imageName, digest)
	if src == nil {
		return nil
	}
	h.adoptSharedBlob(ctx, imageName, src)

	served := *src
	served.Ref = artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}
	return &served
}

// describeSharedReuse is used in logs and tests to make the reuse visible.
func describeSharedReuse(from, to, digest string) string {
	return fmt.Sprintf("blob %s served for %s from bytes already cached as %s", digest, to, from)
}

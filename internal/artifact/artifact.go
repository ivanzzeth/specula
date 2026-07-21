// Package artifact is a compatibility shim that re-exports
// github.com/ivanzzeth/specula/pkg/artifact.
//
// Deprecated: import pkg/artifact directly. This package exists so existing
// internal code keeps compiling during the library migration; it may be
// removed in a future major release.
package artifact

import pkg "github.com/ivanzzeth/specula/pkg/artifact"

type (
	Tier         = pkg.Tier
	Status       = pkg.Status
	ArtifactRef  = pkg.ArtifactRef
	UpstreamMeta = pkg.UpstreamMeta
	Artifact     = pkg.Artifact
	Result       = pkg.Result
	CacheEntry   = pkg.CacheEntry
	MutableEntry = pkg.MutableEntry
	SizeStat     = pkg.SizeStat
)

const (
	TierChecksum  = pkg.TierChecksum
	TierTofu      = pkg.TierTofu
	TierConsensus = pkg.TierConsensus
	TierSigned    = pkg.TierSigned

	StatusPass = pkg.StatusPass
	StatusWarn = pkg.StatusWarn
	StatusFail = pkg.StatusFail
	StatusSkip = pkg.StatusSkip

	OriginCached = pkg.OriginCached
	OriginHosted = pkg.OriginHosted
)

// NormalizeOrigin maps empty/unknown origin to OriginCached.
func NormalizeOrigin(o string) string { return pkg.NormalizeOrigin(o) }

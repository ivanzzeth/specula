// Package meta re-exports the MetadataStore interface and listing helpers.
package meta

import (
	intmeta "github.com/ivanzzeth/specula/internal/store/meta"

	"github.com/ivanzzeth/specula/pkg/artifact"
)

type (
	MetadataStore = intmeta.MetadataStore
	EntryFilter   = intmeta.EntryFilter
	SortField     = intmeta.SortField
	Page          = intmeta.Page
	Entry         = intmeta.Entry
	EntryPage     = intmeta.EntryPage
)

const (
	SortCreatedAt  = intmeta.SortCreatedAt
	SortSize       = intmeta.SortSize
	SortName       = intmeta.SortName
	SortVerifiedAt = intmeta.SortVerifiedAt

	DefaultLimit = intmeta.DefaultLimit
	MaxLimit     = intmeta.MaxLimit
)

var ErrBadEntryID = intmeta.ErrBadEntryID

// EncodeEntryID renders a cache entry's primary key as a URL-safe token.
func EncodeEntryID(ref artifact.ArtifactRef) string {
	return intmeta.EncodeEntryID(ref)
}

// DecodeEntryID reverses EncodeEntryID.
func DecodeEntryID(id string) (artifact.ArtifactRef, error) {
	return intmeta.DecodeEntryID(id)
}

package verify

import (
	"context"
	"time"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/store/meta"
)

// ttlNeverRevalidate matches the daemon's never-revalidate sentinel (-1).
const ttlNeverRevalidate int64 = -1

// MetaTofuStore adapts a MetadataStore into a TofuStore using the mutable tier
// with a "tofu:" key namespace and a never-revalidate TTL so pins are permanent.
type MetaTofuStore struct {
	Meta meta.MetadataStore
}

// NewMetaTofuStore wraps m as a TofuStore.
func NewMetaTofuStore(m meta.MetadataStore) TofuStore {
	return &MetaTofuStore{Meta: m}
}

func (s *MetaTofuStore) GetPin(ctx context.Context, key string) (string, error) {
	e, err := s.Meta.GetMutable(ctx, "tofu:"+key)
	if err != nil {
		return "", err
	}
	if e == nil {
		return "", nil
	}
	return e.Digest, nil
}

func (s *MetaTofuStore) SetPin(ctx context.Context, key, digest string) error {
	return s.Meta.PutMutable(ctx, artifact.MutableEntry{
		Key:        "tofu:" + key,
		Protocol:   "tofu",
		Digest:     digest,
		TTLSeconds: ttlNeverRevalidate,
		FetchedAt:  time.Now().UTC(),
	})
}

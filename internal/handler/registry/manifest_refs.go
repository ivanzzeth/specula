package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	godigest "github.com/opencontainers/go-digest"
)

// manifestRefs is the subset of an OCI image manifest / index used to enforce
// Distribution Spec §"Pushing Manifests": every referenced blob digest must
// already exist in the CAS before the registry accepts the push.
type manifestRefs struct {
	Config *struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
	Subject *struct {
		Digest string `json:"digest"`
	} `json:"subject"`
}

// referencedBlobDigests returns unique, non-empty digests from config, layers,
// index manifests, and subject. Invalid JSON yields nil (caller skips checks —
// putManifest already rejects empty bodies; opaque non-OCI blobs stay allowed).
func referencedBlobDigests(body []byte) []string {
	var refs manifestRefs
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	if refs.Config != nil {
		add(refs.Config.Digest)
	}
	for _, l := range refs.Layers {
		add(l.Digest)
	}
	for _, m := range refs.Manifests {
		add(m.Digest)
	}
	if refs.Subject != nil {
		add(refs.Subject.Digest)
	}
	return out
}

// ensureReferencedBlobsExist verifies every digest in body exists in the CAS.
// Returns the first missing digest, or "" when all present / nothing to check.
// Digests that fail algorithm parsing are reported as missing (client error).
func ensureReferencedBlobsExist(ctx context.Context, blobs interface {
	Exists(context.Context, string) (bool, error)
}, body []byte) (missing string, err error) {
	for _, d := range referencedBlobDigests(body) {
		if err := godigest.Digest(d).Validate(); err != nil {
			return d, nil
		}
		ok, err := blobs.Exists(ctx, d)
		if err != nil {
			return "", fmt.Errorf("blob exists check %s: %w", d, err)
		}
		if !ok {
			return d, nil
		}
	}
	return "", nil
}

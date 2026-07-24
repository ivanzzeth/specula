package registry

import (
	"context"
	"strings"
	"testing"
)

type mapExists map[string]bool

func (m mapExists) Exists(_ context.Context, digest string) (bool, error) {
	return m[digest], nil
}

func TestReferencedBlobDigests(t *testing.T) {
	t.Parallel()
	cfg := "sha256:" + strings.Repeat("1", 64)
	layer := "sha256:" + strings.Repeat("2", 64)
	subj := "sha256:" + strings.Repeat("3", 64)
	idx := "sha256:" + strings.Repeat("4", 64)

	body := []byte(`{
		"schemaVersion":2,
		"config":{"digest":"` + cfg + `","size":1},
		"layers":[{"digest":"` + layer + `","size":1}],
		"subject":{"digest":"` + subj + `","size":1},
		"manifests":[{"digest":"` + idx + `","size":1}]
	}`)
	got := referencedBlobDigests(body)
	want := map[string]bool{cfg: true, layer: true, subj: true, idx: true}
	if len(got) != 4 {
		t.Fatalf("got %d digests %v, want 4", len(got), got)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected digest %s", d)
		}
	}

	if referencedBlobDigests([]byte(`not-json`)) != nil {
		t.Fatal("invalid JSON should yield nil digests")
	}
	if len(referencedBlobDigests([]byte(`{"schemaVersion":2,"layers":[]}`))) != 0 {
		t.Fatal("empty refs should yield no digests")
	}
}

func TestEnsureReferencedBlobsExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := "sha256:" + strings.Repeat("a", 64)
	layer := "sha256:" + strings.Repeat("b", 64)
	store := mapExists{cfg: true, layer: true}

	bodyOK := []byte(`{"config":{"digest":"` + cfg + `"},"layers":[{"digest":"` + layer + `"}]}`)
	missing, err := ensureReferencedBlobsExist(ctx, store, bodyOK)
	if err != nil || missing != "" {
		t.Fatalf("expected ok, missing=%q err=%v", missing, err)
	}

	missingDigest := "sha256:" + strings.Repeat("c", 64)
	bodyMissing := []byte(`{"config":{"digest":"` + cfg + `"},"layers":[{"digest":"` + missingDigest + `"}]}`)
	missing, err = ensureReferencedBlobsExist(ctx, store, bodyMissing)
	if err != nil {
		t.Fatal(err)
	}
	if missing != missingDigest {
		t.Fatalf("missing = %q, want %q", missing, missingDigest)
	}
}

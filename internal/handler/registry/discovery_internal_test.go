package registry

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// TestParseContentRange covers the upload-chunk Content-Range grammar. The OCI
// blob-upload form is a bare inclusive "<start>-<end>" with no unit prefix and
// no "/total" suffix, but the HTTP forms some clients send must parse too.
func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		start, end int64
		ok         bool
	}{
		{"oci bare form", "0-1023", 0, 1023, true},
		{"non-zero start", "1024-2047", 1024, 2047, true},
		{"single byte", "5-5", 5, 5, true},
		{"surrounding space", "  0-1023  ", 0, 1023, true},
		{"http bytes prefix", "bytes 0-1023", 0, 1023, true},
		{"http total suffix", "0-1023/2048", 0, 1023, true},
		{"http prefix and suffix", "bytes 0-1023/2048", 0, 1023, true},

		{"empty", "", 0, 0, false},
		{"no dash", "1024", 0, 0, false},
		{"missing start", "-1023", 0, 0, false},
		{"missing end", "1024-", 0, 0, false},
		{"end before start", "2047-1024", 0, 0, false},
		{"negative start", "-5-10", 0, 0, false},
		{"non-numeric", "abc-def", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := parseContentRange(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseContentRange(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && (start != tc.start || end != tc.end) {
				t.Errorf("parseContentRange(%q) = (%d, %d), want (%d, %d)",
					tc.in, start, end, tc.start, tc.end)
			}
		})
	}
}

// TestPaginateTags covers the ?n= / ?last= tag-listing pagination contract.
func TestPaginateTags(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e"}

	for _, tc := range []struct {
		name      string
		query     string
		want      []string
		truncated bool
		wantErr   bool
	}{
		{"no params returns everything", "", all, false, false},
		{"n caps the page", "n=2", []string{"a", "b"}, true, false},
		{"n equal to length is not truncated", "n=5", all, false, false},
		{"n larger than length", "n=99", all, false, false},
		{"n=0 yields an empty truncated page", "n=0", []string{}, true, false},
		{"last resumes strictly after", "last=b", []string{"c", "d", "e"}, false, false},
		{"last at the end yields nothing", "last=e", []string{}, false, false},
		{"last past the end yields nothing", "last=z", []string{}, false, false},
		{"last before the start keeps everything", "last=A", all, false, false},
		{"last for an absent tag resumes at the next one", "last=bb", []string{"c", "d", "e"}, false, false},
		{"n and last combine", "last=a&n=2", []string{"b", "c"}, true, false},

		{"negative n is invalid", "n=-1", nil, false, true},
		{"non-numeric n is invalid", "n=many", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			page, truncated, err := paginateTags(all, q)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("paginateTags(%q) = nil error, want an error", tc.query)
				}
				return
			}
			if err != nil {
				t.Fatalf("paginateTags(%q): %v", tc.query, err)
			}
			if fmt.Sprint(page) != fmt.Sprint(tc.want) {
				t.Errorf("page = %v, want %v", page, tc.want)
			}
			if truncated != tc.truncated {
				t.Errorf("truncated = %v, want %v", truncated, tc.truncated)
			}
		})
	}
}

// TestNextTagPageLink verifies the rel="next" Link header shape.
func TestNextTagPageLink(t *testing.T) {
	got := nextTagPageLink("org1/repo", "2", "b")
	if !strings.HasPrefix(got, "</v2/org1/repo/tags/list?") {
		t.Errorf("Link = %q, want a /v2/<name>/tags/list URL", got)
	}
	for _, want := range []string{"n=2", "last=b", `rel="next"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Link = %q, want it to contain %q", got, want)
		}
	}

	// Without an explicit n the link carries only the resume cursor.
	got = nextTagPageLink("org1/repo", "", "b")
	if strings.Contains(got, "n=") {
		t.Errorf("Link = %q, want no n parameter when none was requested", got)
	}
}

// TestReadLimited verifies the manifest body cap: at-limit is fine, over-limit
// is an error rather than an unbounded read.
func TestReadLimited(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		max     int64
		wantErr bool
	}{
		{"under the limit", "abc", 10, false},
		{"exactly at the limit", "abcde", 5, false},
		{"one byte over", "abcdef", 5, true},
		{"empty body", "", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readLimited(strings.NewReader(tc.body), tc.max)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readLimited(%q, %d) = nil error, want a limit error", tc.body, tc.max)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLimited: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("got %q, want %q", got, tc.body)
			}
		})
	}
}

// TestEffectiveArtifactType pins the image-spec fallback used when indexing a
// referrer: artifactType, else the config descriptor's mediaType.
func TestEffectiveArtifactType(t *testing.T) {
	m := &manifestMeta{ArtifactType: "application/vnd.example.sbom.v1"}
	m.Config.MediaType = "application/vnd.oci.image.config.v1+json"
	if got := m.effectiveArtifactType(); got != "application/vnd.example.sbom.v1" {
		t.Errorf("got %q, want the explicit artifactType to win", got)
	}

	m = &manifestMeta{}
	m.Config.MediaType = "application/vnd.oci.image.config.v1+json"
	if got := m.effectiveArtifactType(); got != "application/vnd.oci.image.config.v1+json" {
		t.Errorf("got %q, want the config mediaType fallback", got)
	}

	if got := (&manifestMeta{}).effectiveArtifactType(); got != "" {
		t.Errorf("got %q, want empty when neither is set", got)
	}
}

// TestReferrersKeyIsScoped verifies referrers indexes are namespaced per repo
// and per subject, so two repos cannot collide on the same subject digest.
func TestReferrersKeyIsScoped(t *testing.T) {
	a := referrersKey("org1/repo", "sha256:abc")
	b := referrersKey("org2/repo", "sha256:abc")
	c := referrersKey("org1/repo", "sha256:def")

	if a == b {
		t.Error("different repos share a referrers key")
	}
	if a == c {
		t.Error("different subjects share a referrers key")
	}
	// The key must not collide with the tag-pointer namespace ("oci:<name>:<tag>").
	if strings.HasPrefix(a, "oci:") {
		t.Errorf("referrers key %q collides with the oci tag-pointer namespace", a)
	}
}

// TestParseManifestMeta verifies subject extraction and that a non-JSON body is
// simply "no subject" rather than a parse panic.
func TestParseManifestMeta(t *testing.T) {
	m := parseManifestMeta([]byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"subject":{"digest":"sha256:abc","size":10}}`))
	if m == nil || m.Subject == nil || m.Subject.Digest != "sha256:abc" {
		t.Fatalf("parseManifestMeta did not extract the subject: %+v", m)
	}

	m = parseManifestMeta([]byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json"}`))
	if m == nil || m.Subject != nil {
		t.Errorf("manifest without a subject: got %+v, want a non-nil meta with no subject", m)
	}

	if m := parseManifestMeta([]byte(`not json at all`)); m != nil {
		t.Errorf("non-JSON body = %+v, want nil", m)
	}
}

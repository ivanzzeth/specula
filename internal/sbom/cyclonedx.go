// Package sbom builds a CycloneDX cache-inventory BOM from Specula's immutable
// CAS metadata. This is an audit export of what Specula has verified and cached
// — not a deep recursive dependency analysis of package contents.
package sbom

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

const (
	// FormatCycloneDXJSON is the only format supported in v0.12.
	FormatCycloneDXJSON = "cyclonedx-json"
	// SpecVersion is the CycloneDX schema version we emit.
	SpecVersion = "1.5"
	// MaxComponents is the hard ceiling on components in one BOM export so a
	// huge cache cannot exhaust memory on a single admin request.
	MaxComponents = 10000
)

// Options configures a BOM build.
type Options struct {
	// SpeculaVersion is recorded in metadata.tools (may be empty/"dev").
	SpeculaVersion string
	// SerialNumber is a urn:uuid:…; empty → generated from timestamp+count.
	SerialNumber string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Document is a CycloneDX 1.5 BOM (JSON-serialisable).
type Document struct {
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	SerialNumber string       `json:"serialNumber"`
	Version      int          `json:"version"`
	Metadata     Metadata     `json:"metadata"`
	Components   []Component  `json:"components"`
	// Truncated is a Specula extension: true when MaxComponents capped the list.
	// Honest: operators must know the BOM may be incomplete.
	Truncated bool `json:"x-specula-truncated,omitempty"`
}

// Metadata is the CycloneDX metadata block.
type Metadata struct {
	Timestamp string            `json:"timestamp"`
	Tools     []Tool            `json:"tools,omitempty"`
	Component *MetadataComponent `json:"component,omitempty"`
	// Properties carry honest scope notes.
	Properties []Property `json:"properties,omitempty"`
}

// Tool identifies the BOM producer.
type Tool struct {
	Vendor  string `json:"vendor,omitempty"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// MetadataComponent describes the Specula cache as the BOM subject.
type MetadataComponent struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Component is one cached artifact.
type Component struct {
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	PURL       string     `json:"purl,omitempty"`
	Hashes     []Hash     `json:"hashes,omitempty"`
	Properties []Property `json:"properties,omitempty"`
}

// Hash is a CycloneDX hash entry.
type Hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// Property is a CycloneDX name/value property.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FromEntries builds a CycloneDX document from metadata listing rows.
// Mutable refs are skipped (indexes/packuments are not package payloads).
// At most MaxComponents immutable entries are included; Truncated is set if more
// were present in the input slice after filtering.
func FromEntries(entries []meta.Entry, opts Options) Document {
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	ver := strings.TrimSpace(opts.SpeculaVersion)
	if ver == "" {
		ver = "dev"
	}
	serial := strings.TrimSpace(opts.SerialNumber)
	if serial == "" {
		serial = fmt.Sprintf("urn:uuid:specula-%d", now.UnixNano())
	}

	comps := make([]Component, 0, len(entries))
	skippedMutable := 0
	for _, e := range entries {
		if e.Ref.Mutable {
			skippedMutable++
			continue
		}
		comps = append(comps, componentFromEntry(e))
		if len(comps) >= MaxComponents {
			break
		}
	}
	truncated := false
	immutable := 0
	for _, e := range entries {
		if !e.Ref.Mutable {
			immutable++
		}
	}
	if immutable > MaxComponents {
		truncated = true
	}

	return Document{
		BOMFormat:    "CycloneDX",
		SpecVersion:  SpecVersion,
		SerialNumber: serial,
		Version:      1,
		Metadata: Metadata{
			Timestamp: now.Format(time.RFC3339),
			Tools: []Tool{{
				Vendor:  "specula",
				Name:    "specula",
				Version: ver,
			}},
			Component: &MetadataComponent{
				Type: "application",
				Name: "specula-cache-inventory",
			},
			Properties: []Property{
				{Name: "specula:bom-kind", Value: "cache-inventory"},
				{Name: "specula:note", Value: "Lists verified immutable CAS entries Specula has cached; not a recursive dependency graph of package contents."},
				{Name: "specula:skipped-mutable", Value: fmt.Sprintf("%d", skippedMutable)},
			},
		},
		Components: comps,
		Truncated:  truncated,
	}
}

func componentFromEntry(e meta.Entry) Component {
	ref := e.Ref
	c := Component{
		Type:    componentType(ref.Protocol),
		Name:    ref.Name,
		Version: ref.Version,
		PURL:    PackageURL(ref),
		Properties: []Property{
			{Name: "specula:protocol", Value: ref.Protocol},
			{Name: "specula:tier", Value: e.Tier.String()},
		},
	}
	if e.Upstream != "" {
		c.Properties = append(c.Properties, Property{Name: "specula:upstream", Value: e.Upstream})
	}
	if hex := sha256Hex(e.Digest); hex != "" {
		c.Hashes = []Hash{{Alg: "SHA-256", Content: hex}}
	}
	return c
}

func componentType(protocol string) string {
	switch protocol {
	case "oci":
		return "container"
	case "git":
		return "file"
	default:
		return "library"
	}
}

// PackageURL builds a best-effort package URL for the artifact identity.
// When the protocol has no stable PURL mapping, returns a generic PURL rather
// than inventing a wrong ecosystem.
func PackageURL(ref artifact.ArtifactRef) string {
	name := strings.TrimSpace(ref.Name)
	ver := strings.TrimSpace(ref.Version)
	if name == "" {
		return ""
	}
	switch ref.Protocol {
	case "npm":
		return "pkg:npm/" + encodeNPMName(name) + purlVersion(ver)
	case "pypi":
		return "pkg:pypi/" + url.PathEscape(name) + purlVersion(stripPyPIFilename(name, ver))
	case "gomod", "go":
		return "pkg:golang/" + encodePath(name) + purlVersion(ver)
	case "cargo":
		return "pkg:cargo/" + url.PathEscape(name) + purlVersion(ver)
	case "oci":
		if dig := sha256Hex(ref.Digest); dig != "" {
			return "pkg:oci/" + encodePath(name) + "@sha256:" + dig
		}
		return "pkg:oci/" + encodePath(name) + purlVersion(ver)
	case "apt":
		return "pkg:generic/apt/" + encodePath(name) + purlVersion(ver)
	case "helm":
		return "pkg:generic/helm/" + encodePath(name) + purlVersion(ver)
	case "tarball":
		return "pkg:generic/tarball/" + encodePath(name) + purlVersion(ver)
	case "conda":
		return "pkg:generic/conda/" + encodePath(name) + purlVersion(ver)
	case "hf":
		return "pkg:generic/huggingface/" + encodePath(name) + purlVersion(ver)
	default:
		return "pkg:generic/" + url.PathEscape(ref.Protocol) + "/" + encodePath(name) + purlVersion(ver)
	}
}

func purlVersion(ver string) string {
	if ver == "" {
		return ""
	}
	return "@" + url.PathEscape(ver)
}

func encodeNPMName(name string) string {
	// Scoped: @scope/pkg → %40scope/pkg (Package URL convention).
	if strings.HasPrefix(name, "@") {
		if i := strings.IndexByte(name, '/'); i > 0 {
			scope := name[1:i] // strip leading @
			pkg := name[i+1:]
			return "%40" + url.PathEscape(scope) + "/" + url.PathEscape(pkg)
		}
		return "%40" + url.PathEscape(name[1:])
	}
	return url.PathEscape(name)
}

func encodePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func sha256Hex(digest string) string {
	digest = strings.TrimSpace(digest)
	if strings.HasPrefix(digest, "sha256:") {
		return strings.TrimPrefix(digest, "sha256:")
	}
	return ""
}

// stripPyPIFilename turns a wheel/sdist filename back toward a version when
// possible; otherwise returns the filename unchanged (honest).
func stripPyPIFilename(pkg, file string) string {
	if file == "" {
		return ""
	}
	// Already a plain version.
	if !strings.Contains(file, "-") || (!strings.HasSuffix(file, ".whl") &&
		!strings.HasSuffix(file, ".tar.gz") && !strings.HasSuffix(file, ".zip")) {
		return file
	}
	base := file
	for _, ext := range []string{".tar.gz", ".whl", ".zip", ".tar.bz2", ".egg"} {
		if strings.HasSuffix(base, ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	prefix := strings.ReplaceAll(pkg, "-", "_") + "-"
	// Wheel uses underscore-normalized distribution name; also try pkg-.
	for _, p := range []string{prefix, pkg + "-", strings.ToLower(pkg) + "-"} {
		if strings.HasPrefix(strings.ToLower(base), strings.ToLower(p)) {
			rest := base[len(p):]
			// Wheel: version is before next -{python}
			if i := strings.IndexByte(rest, '-'); i > 0 {
				return rest[:i]
			}
			return rest
		}
	}
	return file
}

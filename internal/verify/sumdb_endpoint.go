package verify

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"golang.org/x/mod/sumdb/note"
)

// DefaultSumDBName is the checksum-database name the go command uses unless
// GOSUMDB / GONOSUMDB says otherwise. sum.golang.google.cn is a CN-reachable
// CDN mirror of the SAME transparency log, so it still signs notes as
// "sum.golang.org" — the name is a property of the log, not of the host.
const DefaultSumDBName = "sum.golang.org"

// SumDBNameFromKey extracts the checksum-database name from a note verifier key
// ("<name>+<id>+<pubkey>"). An empty or unparseable key yields DefaultSumDBName,
// matching the compiled default the verifier falls back to.
func SumDBNameFromKey(vkeyText string) string {
	vkeyText = strings.TrimSpace(vkeyText)
	if vkeyText == "" {
		return DefaultSumDBName
	}
	v, err := note.NewVerifier(vkeyText)
	if err != nil {
		return DefaultSumDBName
	}
	return v.Name()
}

// SumDBEndpoint resolves upstream URLs for a checksum database. It exists because
// the single `sumdb.url` config key has two legitimate, mutually incompatible
// wire shapes, and the verifier and the /sumdb/ passthrough were each hard-coding
// a different one — so no config value worked for both (BUG A).
//
// The two shapes, measured against the real CN hosts:
//
//	DIRECT — the checksum database itself, served at its host root.
//	  https://sum.golang.google.cn/lookup/rsc.io/quote@v1.5.2   200
//	  https://sum.golang.google.cn/latest                       200
//	  https://sum.golang.google.cn/supported                    404  (not a sumdb endpoint)
//	  https://sum.golang.google.cn/sum.golang.org/lookup/...    404  (no name segment)
//
//	PROXY — a GOPROXY module-proxy "/sumdb" base, which routes on the db name.
//	  https://goproxy.cn/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2   200
//	  https://goproxy.cn/sumdb/sum.golang.org/supported                    200
//	  https://goproxy.cn/sumdb/lookup/rsc.io/quote@v1.5.2                  404  (name missing)
//
// The distinction is detected from the URL path, not guessed: the module proxy
// protocol fixes the literal path element "sumdb" as the prefix of a proxy's
// checksum-database endpoints (GOPROXY/sumdb/<sumdb-name>/<path>). A base URL
// whose path ends in "/sumdb" is therefore unambiguously a proxy base; a
// checksum database is never itself served at a path ending in "/sumdb".
//
// Both callers resolve through URL, so both shapes work for both callers, and a
// change to the convention can only be made in one place.
type SumDBEndpoint struct {
	// base is the configured URL with any trailing slash removed.
	base string
	// proxy is true for a GOPROXY "/sumdb" base (name goes in the path),
	// false for a direct sumdb host (name is not part of its URL space).
	proxy bool
}

// ParseSumDBEndpoint classifies rawURL into one of the two shapes above.
// An empty rawURL yields the compiled default (the direct sum.golang.org host).
func ParseSumDBEndpoint(rawURL string) (SumDBEndpoint, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		raw = "https://" + DefaultSumDBName
	}
	u, err := url.Parse(raw)
	if err != nil {
		return SumDBEndpoint{}, fmt.Errorf("sumdb: bad url %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return SumDBEndpoint{}, fmt.Errorf("sumdb: url %q must be http or https, got scheme %q", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return SumDBEndpoint{}, fmt.Errorf("sumdb: url %q has no host", rawURL)
	}
	trimmed := strings.TrimRight(u.Path, "/")
	return SumDBEndpoint{
		base:  strings.TrimRight(raw, "/"),
		proxy: path.Base(trimmed) == "sumdb",
	}, nil
}

// IsProxyStyle reports whether this endpoint is a GOPROXY "/sumdb" base.
func (e SumDBEndpoint) IsProxyStyle() bool { return e.proxy }

// Base returns the configured base URL (no trailing slash).
func (e SumDBEndpoint) Base() string { return e.base }

// URL resolves the upstream URL for a checksum-database endpoint path.
//
// name is the sumdb name (e.g. "sum.golang.org"); elem is the database-relative
// endpoint path with a leading slash, exactly as x/mod's sumdb.ClientOps hands it
// over: "/lookup/<module>@<version>", "/tile/...", "/latest".
//
// For a proxy base the name is a required routing segment; for a direct sumdb it
// must be omitted — that asymmetry is the whole point of this type.
func (e SumDBEndpoint) URL(name, elem string) string {
	if !strings.HasPrefix(elem, "/") {
		elem = "/" + elem
	}
	if e.proxy {
		return e.base + "/" + strings.Trim(name, "/") + elem
	}
	return e.base + elem
}

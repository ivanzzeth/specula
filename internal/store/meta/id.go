package meta

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ErrBadEntryID is returned by DecodeEntryID when the input is not a
// well-formed entry identifier.
var ErrBadEntryID = errors.New("meta: malformed cache entry id")

// entryIDSep separates the three primary-key components inside an encoded entry
// ID. NUL is used because it cannot appear in a protocol, name or version, so
// the encoding is unambiguous without escaping.
const entryIDSep = "\x00"

// EncodeEntryID renders a cache entry's (protocol, name, version) primary key as
// a single opaque, URL-safe token.
//
// The reason this exists rather than exposing the three columns as path segments:
// artifact names routinely contain "/" (OCI "library/nginx", Go module paths,
// npm scopes), which would shred a REST path. Base64url of the NUL-joined tuple
// is path-safe, needs no percent-encoding, and round-trips exactly.
//
// The result is an identifier, not a secret: it is deterministic and trivially
// decodable by design, so the UI may treat it as a stable React key. Never rely
// on it to hide anything — authorization for delete/pin is enforced separately.
func EncodeEntryID(ref artifact.ArtifactRef) string {
	joined := strings.Join([]string{ref.Protocol, ref.Name, ref.Version}, entryIDSep)
	return base64.RawURLEncoding.EncodeToString([]byte(joined))
}

// DecodeEntryID reverses EncodeEntryID, returning a ref carrying only the
// primary-key fields (Protocol, Name, Version) — enough to Get / Delete /
// SetPinned the entry.
//
// It fails closed: any input that is not valid base64url, or that does not
// decode to exactly three NUL-separated components, or whose protocol/name is
// empty, yields ErrBadEntryID rather than a partially-populated ref that a
// caller might use to address the wrong row.
func DecodeEntryID(id string) (artifact.ArtifactRef, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return artifact.ArtifactRef{}, ErrBadEntryID
	}
	parts := strings.Split(string(raw), entryIDSep)
	if len(parts) != 3 {
		return artifact.ArtifactRef{}, ErrBadEntryID
	}
	if parts[0] == "" || parts[1] == "" {
		return artifact.ArtifactRef{}, ErrBadEntryID
	}
	return artifact.ArtifactRef{
		Protocol: parts[0],
		Name:     parts[1],
		Version:  parts[2],
	}, nil
}

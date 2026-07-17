package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAllProtocolsMatchesOnTheWireProtocolNames pins the protocol label
// vocabulary to the ArtifactRef.Protocol values the rest of the system actually
// uses — NOT to the config keys, which differ.
//
// This is a real bug that shipped and was caught only by the real-traffic
// acceptance run: cmd/specula pre-initialised specula_upstream_blocked under the
// CONFIG key "go", while the upstream client labels it from ref.Protocol =
// "gomod". The result was a phantom {protocol="go"} series reading 0 for ever,
// with any genuine block landing on a different series entirely. An operator
// watching the phantom sees a permanently healthy upstream.
//
// The label vocabulary is therefore stated once, here, and asserted to contain
// the on-the-wire name.
func TestAllProtocolsMatchesOnTheWireProtocolNames(t *testing.T) {
	in := func(s string) bool {
		for _, p := range AllProtocols {
			if p == s {
				return true
			}
		}
		return false
	}

	// gomod.Protocol == "gomod"; the config key is "go". The label must be the
	// former. Asserted by value rather than by importing the handler package,
	// which would invert the dependency direction of this leaf package.
	assert.True(t, in("gomod"), "protocol label must be the on-the-wire %q (gomod.Protocol), not the config key", "gomod")
	assert.False(t, in("go"), `"go" is the CONFIG key, never a protocol label — pre-initialising under it mints a phantom series that reads 0 for ever`)

	for _, p := range []string{"oci", "pypi", "npm", "apt", "helm", "tarball", "git"} {
		assert.True(t, in(p), "protocol %q must be in the label vocabulary", p)
	}
	assert.Len(t, AllProtocols, 8, "PRD §2 fixes the protocol list at eight")
}

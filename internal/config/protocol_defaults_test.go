package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeCfg writes a minimal loadable config with the given extra YAML appended.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	return writeCfgHA(t, false, body)
}

// writeCfgHA is writeCfg with server.ha set, since a second `server:` key of our
// own would be a duplicate mapping key rather than a merge.
func writeCfgHA(t *testing.T, ha bool, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	haLine := ""
	if ha {
		haLine = "  ha: true\n"
	}
	base := "server:\n  listen_addr: \"127.0.0.1:7733\"\n" + haLine +
		"storage:\n  blob:\n    driver: local\n    local:\n      root: " + dir + "/blobs\n" +
		"  meta:\n    driver: sqlite\n    dsn: " + dir + "/meta.db\n"
	require.NoError(t, os.WriteFile(path, []byte(base+body), 0o644))
	return path
}

// The bug this whole file exists for: a config that configures only OCI — which
// is every chart and ConfigMap we ship — served OCI and 404'd everything else.
func TestLoadServesEveryProtocolWhenConfigNamesOnlyOCI(t *testing.T) {
	path := writeCfg(t, `
protocols:
  oci:
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	for _, proto := range []string{"oci", "npm", "pypi", "go", "apt", "helm", "cargo", "conda", "hf"} {
		pc, ok := cfg.Protocols[proto]
		require.True(t, ok, "protocol %q must be served without being named in the config", proto)
		require.NotEmpty(t, pc.Upstreams, "protocol %q defaulted with no upstream chain", proto)
	}
}

func TestLoadServesEveryProtocolWhenConfigHasNoProtocolsBlockAtAll(t *testing.T) {
	cfg, err := Load(writeCfg(t, ""))
	require.NoError(t, err)

	names, err := DefaultProtocolNames()
	require.NoError(t, err)
	require.NotEmpty(t, names)
	for _, n := range names {
		require.Contains(t, cfg.Protocols, n)
	}
}

// The operator's config always wins over the defaults.
func TestOperatorUpstreamsAreNotOverriddenByDefaults(t *testing.T) {
	path := writeCfg(t, `
protocols:
  npm:
    upstreams:
      - name: house-mirror
        base_url: https://npm.internal.example.com
        priority: 1
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	npm := cfg.Protocols["npm"]
	require.Len(t, npm.Upstreams, 1, "defaults must not be appended to an operator's chain")
	require.Equal(t, "house-mirror", npm.Upstreams[0].Name)
}

// A block that only retunes a TTL must not have to restate the mirror list, and
// must not fail Validate's "at least one upstream is required".
func TestProtocolBlockWithoutUpstreamsInheritsTheDefaultChain(t *testing.T) {
	path := writeCfg(t, `
protocols:
  pypi:
    mutable_ttl_seconds: 60
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	pypi := cfg.Protocols["pypi"]
	require.NotEmpty(t, pypi.Upstreams)
	require.NotNil(t, pypi.MutableTTLSeconds)
	require.EqualValues(t, 60, *pypi.MutableTTLSeconds)
}

// Explicit opt-out is the only way off.
func TestEnabledFalseRemovesTheProtocol(t *testing.T) {
	path := writeCfg(t, `
protocols:
  hf:
    enabled: false
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	_, ok := cfg.Protocols["hf"]
	require.False(t, ok, "enabled:false must remove the protocol so no handler is registered")
	require.Contains(t, cfg.Protocols, "npm", "disabling one protocol must not disable the rest")
}

// enabled:false with no upstreams must not trip validation on its way out.
func TestEnabledFalseWithNoUpstreamsIsNotAValidationError(t *testing.T) {
	path := writeCfg(t, `
protocols:
  apt:
    enabled: false
  conda:
    enabled: false
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotContains(t, cfg.Protocols, "apt")
	require.NotContains(t, cfg.Protocols, "conda")
}

func TestEnabledTrueIsAcceptedAndKeepsTheProtocol(t *testing.T) {
	path := writeCfg(t, `
protocols:
  npm:
    enabled: true
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	npm, ok := cfg.Protocols["npm"]
	require.True(t, ok)
	require.NotEmpty(t, npm.Upstreams)
}

// git keeps bare mirrors on local disk, so it must not switch itself on under HA.
func TestGitIsNotAutoEnabledUnderHA(t *testing.T) {
	cfg := &Config{}
	cfg.Server.HA = true
	require.NoError(t, applyProtocolDefaults(cfg, nil))
	require.NotEmpty(t, cfg.Protocols, "HA must still default the other protocols on")
	require.NotContains(t, cfg.Protocols, "git",
		"git bare mirrors are node-local; auto-enabling them under HA answers from whichever replica you hit")
}

// Defaults must carry the CN-first chain with an official upstream as fallback —
// that ordering is the product.
func TestDefaultsAreChinaFirstWithOfficialFallback(t *testing.T) {
	table, err := DefaultProtocols()
	require.NoError(t, err)

	for _, proto := range []string{"oci", "npm", "pypi", "go"} {
		pc, ok := table[proto]
		require.True(t, ok, "protocol %q missing from defaults", proto)
		require.GreaterOrEqual(t, len(pc.Upstreams), 2,
			"%q needs a mirror plus an official fallback", proto)

		var sawOfficial bool
		for _, up := range pc.Upstreams {
			if up.Official {
				sawOfficial = true
			}
		}
		require.True(t, sawOfficial, "%q has no official upstream to fall back to", proto)
		require.False(t, pc.Upstreams[0].Official,
			"%q tries the official upstream first, which is the one CN cannot reach", proto)
	}
}

// A default with no upstreams could not serve anything and would fail Validate.
func TestDefaultsNeverCarryAnEmptyUpstreamChain(t *testing.T) {
	table, err := DefaultProtocols()
	require.NoError(t, err)
	for name, pc := range table {
		require.NotEmpty(t, pc.Upstreams, "default %q has an empty chain", name)
	}
}

// The defaults come from example.yaml; if that file grows a key ProtocolConfig
// has no field for, this fails rather than silently dropping it from every
// default.
func TestDefaultsParseTheEmbeddedExampleStrictly(t *testing.T) {
	_, err := DefaultProtocols()
	require.NoError(t, err)
}

func TestApplyProtocolDefaultsIsNilSafeOnProtocolsMap(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, applyProtocolDefaults(cfg, nil))
	require.NotEmpty(t, cfg.Protocols)
}

// `upstreams: []` is not the same as an omitted key: the operator said "no
// chain", so they get the validation error and both ways out, not a silent
// substitution of ours.
func TestExplicitEmptyUpstreamsStaysAValidationError(t *testing.T) {
	path := writeCfg(t, `
protocols:
  npm:
    upstreams: []
`)
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "protocols.npm")
	require.Contains(t, err.Error(), "at least one upstream")
	require.Contains(t, err.Error(), "enabled: false",
		"the error must name the way to stop serving a protocol")
}

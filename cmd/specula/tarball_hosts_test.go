package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/config"
)

// Helm rewrites absolute chart urls to ../../tarball/<host>/…. If the tarball
// SSRF allowlist only reads protocols.tarball.upstreams, a helm-only config
// 403s every helm pull. Allowlist must union helm upstream + repo hosts.
func TestTarballAllowlistHosts_UnionsHelmHosts(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"helm": {
				Upstreams: []config.UpstreamConfig{
					{Name: "azure-cn", BaseURL: "https://mirror.azure.cn/kubernetes"},
				},
				Helm: &config.HelmConfig{
					Repositories: []config.NamedSource{
						{Name: "charts", BaseURL: "https://mirror.azure.cn/kubernetes/charts"},
						{Name: "longhorn", BaseURL: "https://charts.longhorn.io"},
					},
				},
			},
		},
	}

	hosts := tarballAllowlistHosts(cfg)
	assert.Contains(t, hosts, "mirror.azure.cn")
	assert.Contains(t, hosts, "charts.longhorn.io")
	assert.Len(t, hosts, 2, "same host from upstream+repo must be deduped")
}

func TestTarballAllowlistHosts_UnionsExplicitTarballUpstreams(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"tarball": {
				Upstreams: []config.UpstreamConfig{
					{Name: "gh", BaseURL: "https://github.com"},
				},
			},
			"helm": {
				Upstreams: []config.UpstreamConfig{
					{Name: "azure", BaseURL: "https://mirror.azure.cn/kubernetes"},
				},
			},
		},
	}

	hosts := tarballAllowlistHosts(cfg)
	require.ElementsMatch(t, []string{"github.com", "mirror.azure.cn"}, hosts)
}

func TestTarballAllowlistHosts_EmptyWhenNeitherConfigured(t *testing.T) {
	assert.Empty(t, tarballAllowlistHosts(&config.Config{}))
	assert.Empty(t, tarballAllowlistHosts(&config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"npm": {Upstreams: []config.UpstreamConfig{{BaseURL: "https://registry.npmjs.org"}}},
		},
	}))
}

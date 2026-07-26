package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOCIRemoteRegistriesNeedCNUpgrade(t *testing.T) {
	t.Run("single protocol upstream", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {Upstreams: []UpstreamConfig{{Name: "only", BaseURL: "https://docker.m.daocloud.io"}}},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("legacy base_url only", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
				{Host: "ghcr.io", BaseURL: "https://ghcr.m.daocloud.io"},
			}}},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("single remote upstream", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
				{Host: "ghcr.io", Upstreams: []UpstreamConfig{{Name: "daocloud", BaseURL: "https://ghcr.m.daocloud.io"}}},
			}}},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("multi-mirror healthy", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "ghcr.io", Upstreams: []UpstreamConfig{
						{Name: "daocloud", BaseURL: "https://ghcr.m.daocloud.io"},
						{Name: "1ms", BaseURL: "https://ghcr.1ms.run"},
					}},
				}},
			},
		}}
		assert.False(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})
}

func TestShouldOverwriteOCIOnInstall_EnvOverride(t *testing.T) {
	cfg := &Config{Protocols: map[string]ProtocolConfig{
		"oci": {
			Upstreams: []UpstreamConfig{
				{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
				{Name: "1ms", BaseURL: "https://docker.1ms.run"},
			},
		},
	}}
	t.Setenv("SPECULA_FORCE_CN_OCI", "1")
	assert.True(t, ShouldOverwriteOCIOnInstall(cfg))
	t.Setenv("SPECULA_FORCE_CN_OCI", "0")
	assert.False(t, ShouldOverwriteOCIOnInstall(cfg))
}

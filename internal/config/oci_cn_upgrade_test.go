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

	t.Run("k8s without huawei-ddn needs upgrade", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "registry.k8s.io", Upstreams: []UpstreamConfig{
						{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("k8s.gcr.io without nested mirror needs upgrade", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "k8s.gcr.io", Upstreams: []UpstreamConfig{
						{Name: "daocloud", BaseURL: "https://k8s-gcr.m.daocloud.io"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("k8s with huawei-ddn layout on SWR is current", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "registry.k8s.io", Upstreams: []UpstreamConfig{
						{Name: "huawei-swr", BaseURL: "https://swr.cn-north-4.myhuaweicloud.com", Layout: "huawei-ddn"},
						{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.False(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("k8s.gcr.io with huawei-ddn on SWR is current", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "k8s.gcr.io", Upstreams: []UpstreamConfig{
						{Name: "huawei-swr", BaseURL: "https://swr.cn-north-4.myhuaweicloud.com", Layout: "huawei-ddn"},
						{Name: "daocloud", BaseURL: "https://k8s-gcr.m.daocloud.io"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.False(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("huawei-ddn layout on transparent mirror does not count", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "registry.k8s.io", Upstreams: []UpstreamConfig{
						{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io", Layout: "huawei-ddn"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("custom path_prefix without ddn-k8s counts as current", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "registry.k8s.io", Upstreams: []UpstreamConfig{
						{Name: "custom", BaseURL: "https://mirror.example", PathPrefix: "org/registry.k8s.io"},
						{Name: "1ms", BaseURL: "https://k8s.1ms.run"},
					}},
				}},
			},
		}}
		assert.False(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
	})

	t.Run("SWR base without layout or path_prefix needs upgrade", func(t *testing.T) {
		cfg := &Config{Protocols: map[string]ProtocolConfig{
			"oci": {
				Upstreams: []UpstreamConfig{
					{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io"},
					{Name: "1ms", BaseURL: "https://docker.1ms.run"},
				},
				OCI: &OCIConfig{RemoteRegistries: []OCIRemoteRegistry{
					{Host: "registry.k8s.io", Upstreams: []UpstreamConfig{
						{Name: "huawei-swr", BaseURL: "https://swr.cn-north-4.myhuaweicloud.com"},
						{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io"},
					}},
				}},
			},
		}}
		assert.True(t, OCIRemoteRegistriesNeedCNUpgrade(cfg))
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

func TestIsHuaweiSWRBaseURL(t *testing.T) {
	assert.True(t, isHuaweiSWRBaseURL("https://swr.cn-north-4.myhuaweicloud.com"))
	assert.True(t, isHuaweiSWRBaseURL("https://swr.cn-east-3.myhuaweicloud.com/v2"))
	assert.False(t, isHuaweiSWRBaseURL("https://k8s.m.daocloud.io"))
	assert.False(t, isHuaweiSWRBaseURL("https://repo.huaweicloud.com/repository/npm"))
	assert.False(t, isHuaweiSWRBaseURL("not-a-url"))
}

package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/config"
)

func TestUpgradeHintsEmptyAllowlists(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"apt":  {Apt: &config.AptConfig{}},
			"helm": {Helm: &config.HelmConfig{}},
			"oci":  {OCI: &config.OCIConfig{}},
		},
	}
	hints := config.UpgradeHints(cfg)
	require.Len(t, hints, 3)
	sections := map[string]bool{}
	for _, h := range hints {
		sections[h.Section] = true
		assert.Contains(t, h.Message, "apply-example")
	}
	assert.True(t, sections["apt"] && sections["helm"] && sections["oci"])
}

func TestUpgradeHintsSilentWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"apt": {
				Apt: &config.AptConfig{
					Repositories: []config.NamedSource{{Name: "ubuntu", BaseURL: "https://archive.ubuntu.com/ubuntu"}},
				},
			},
		},
	}
	assert.Empty(t, config.UpgradeHints(cfg))
}

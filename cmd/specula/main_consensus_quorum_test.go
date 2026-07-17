package main

// main_consensus_quorum_test.go — BUG C regression: an UNSATISFIABLE consensus
// config is accepted silently, then 502s forever.
//
// buildConsensusVerifier derives mirrors from the protocol upstreams: non-official
// upstreams become mirrors, and the first `official` one becomes the origin
// WITNESS — not a mirror. So the reference shape
//
//	pypi:
//	  upstreams:
//	    - {name: tuna,   base_url: https://pypi.tuna.tsinghua.edu.cn/simple}
//	    - {name: pypi,   base_url: https://pypi.org/simple, official: true}
//	  verification: {tiers: [consensus], quorum: 2}
//
// yields mirrors=1 with quorum=2 → quorum can never be met → every pypi fetch
// 502s forever, while startup cheerfully logs "quorum:2 mirrors:1".
//
// A security tier that silently cannot pass is worse than one that is off.

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/verify"
)

// referencePyPIConfig is the shape from the docs: one CN mirror + the official
// origin marked `official: true`, asking for quorum 2.
func referencePyPIConfig(quorum int) *config.Config {
	return &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"pypi": {
				Upstreams: []config.UpstreamConfig{
					{Name: "tuna", BaseURL: "https://pypi.tuna.tsinghua.edu.cn/simple"},
					{Name: "pypi", BaseURL: "https://pypi.org/simple", Official: true},
				},
				Verification: config.VerificationConfig{
					Tiers:  []string{"consensus"},
					Quorum: quorum,
				},
			},
		},
	}
}

// TestBuildConsensusVerifier_QuorumExceedsMirrors_Fails is the primary RED test
// for BUG C.
func TestBuildConsensusVerifier_QuorumExceedsMirrors_Fails(t *testing.T) {
	cfg := referencePyPIConfig(2) // 2 upstreams, but one is the origin witness → 1 mirror

	_, err := buildConsensusVerifier("pypi", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())

	require.Error(t, err,
		"BUG C: quorum=2 over 1 derived mirror is unsatisfiable — the consensus tier can "+
			"never pass and every pypi fetch 502s forever. This must be rejected at wire time, "+
			"not logged as 'quorum:2 mirrors:1' and accepted.")
	assert.Contains(t, err.Error(), "quorum")
	assert.Contains(t, err.Error(), "2", "the error must name the configured quorum")
	assert.Contains(t, err.Error(), "1", "the error must name the derived mirror count")
	assert.Contains(t, err.Error(), "pypi", "the error must name the protocol")
}

// TestBuildConsensusVerifier_SatisfiableQuorum_OK pins the working shape: two
// independent mirrors + the official origin as witness.
func TestBuildConsensusVerifier_SatisfiableQuorum_OK(t *testing.T) {
	cfg := referencePyPIConfig(2)
	pc := cfg.Protocols["pypi"]
	pc.Upstreams = append(pc.Upstreams, config.UpstreamConfig{
		Name: "aliyun", BaseURL: "https://mirrors.aliyun.com/pypi/simple",
	})
	cfg.Protocols["pypi"] = pc

	v, err := buildConsensusVerifier("pypi", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())
	require.NoError(t, err)
	assert.NotNil(t, v, "quorum=2 over 2 independent mirrors is satisfiable")
}

// TestBuildConsensusVerifier_ExplicitMirrors_QuorumExceeds_Fails covers the
// structured consensus block, where mirrors are listed rather than derived.
func TestBuildConsensusVerifier_ExplicitMirrors_QuorumExceeds_Fails(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"pypi": {
				Verification: config.VerificationConfig{
					Consensus: &config.ConsensusConfig{
						Quorum: 3,
						Mirrors: []config.ConsensusMirrorConfig{
							{Name: "tuna", BaseURL: "https://pypi.tuna.tsinghua.edu.cn/simple"},
						},
					},
				},
			},
		},
	}

	_, err := buildConsensusVerifier("pypi", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())
	require.Error(t, err, "quorum=3 over 1 explicitly listed mirror is unsatisfiable")
	assert.Contains(t, err.Error(), "3")
	assert.Contains(t, err.Error(), "1")
}

// TestBuildConsensusVerifier_NoMirrors_Fails: consensus asked for, zero mirrors
// derivable — also unsatisfiable, and the message must say so.
func TestBuildConsensusVerifier_NoMirrors_Fails(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"pypi": {
				Upstreams: []config.UpstreamConfig{
					{Name: "pypi", BaseURL: "https://pypi.org/simple", Official: true},
				},
				Verification: config.VerificationConfig{Tiers: []string{"consensus"}, Quorum: 1},
			},
		},
	}
	_, err := buildConsensusVerifier("pypi", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())
	require.Error(t, err, "consensus with zero independent mirrors can never pass")
}

// TestBuildConsensusVerifier_NotEnabled_NoError: consensus not requested → nil, nil.
func TestBuildConsensusVerifier_NotEnabled_NoError(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"pypi": {Verification: config.VerificationConfig{Tiers: []string{"tofu"}}},
		},
	}
	v, err := buildConsensusVerifier("pypi", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestBuildConsensusVerifier_MetadataOnlyImpossible_NoError: npm/tarball cannot do
// metadata-only consensus; that is a documented downgrade to tofu, not an error.
func TestBuildConsensusVerifier_MetadataOnlyImpossible_NoError(t *testing.T) {
	cfg := &config.Config{
		Protocols: map[string]config.ProtocolConfig{
			"npm": {
				Upstreams: []config.UpstreamConfig{
					{Name: "npmmirror", BaseURL: "https://registry.npmmirror.com"},
				},
				Verification: config.VerificationConfig{Tiers: []string{"consensus"}, Quorum: 5},
			},
		},
	}
	v, err := buildConsensusVerifier("npm", cfg, verify.NewHTTPMirrorDigestFetcher(0), slog.Default())
	require.NoError(t, err, "npm consensus is documented as unachievable metadata-only → tofu")
	assert.Nil(t, v)
}

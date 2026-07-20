// Package verify re-exports the streaming verification chain and common verifiers.
package verify

import (
	"time"

	intverify "github.com/ivanzzeth/specula/internal/verify"
)

type (
	Verifier            = intverify.Verifier
	Chain               = intverify.Chain
	TofuStore           = intverify.TofuStore
	TreeSizeStore       = intverify.TreeSizeStore
	AptPinStore         = intverify.AptPinStore
	MirrorDigestFetcher = intverify.MirrorDigestFetcher
	SignatureFetcher    = intverify.SignatureFetcher
	PrivateMatcher      = intverify.PrivateMatcher
	SumDBConfig         = intverify.SumDBConfig
	ConsensusConfig     = intverify.ConsensusConfig
	ConsensusMirror     = intverify.ConsensusMirror
	OriginCheck         = intverify.OriginCheck
	CosignConfig        = intverify.CosignConfig
	GPGVerifier         = intverify.GPGVerifier
	HelmProvVerifier    = intverify.HelmProvVerifier
	GitSignedVerifier   = intverify.GitSignedVerifier
	SumDBVerifier       = intverify.SumDBVerifier
	CosignVerifier      = intverify.CosignVerifier
	ConsensusVerifier   = intverify.ConsensusVerifier
	Policy              = intverify.Policy
)

const (
	PolicyEnforce = intverify.PolicyEnforce
	PolicyWarn    = intverify.PolicyWarn
)

// NewChain builds a verification Chain from an ordered list of verifiers.
func NewChain(verifiers ...Verifier) *Chain {
	return intverify.NewChain(verifiers...)
}

// NewChecksumVerifier returns the transport-integrity verifier (TierChecksum).
func NewChecksumVerifier() Verifier {
	return intverify.NewChecksumVerifier()
}

// NewTofuVerifier returns the first-seen digest pin verifier (TierTofu).
func NewTofuVerifier(store TofuStore) Verifier {
	return intverify.NewTofuVerifier(store)
}

// NewGPGVerifier constructs the apt InRelease/Packages GPG verifier.
func NewGPGVerifier(keyringPath string, opts ...GPGOption) (*GPGVerifier, error) {
	return intverify.NewGPGVerifier(keyringPath, opts...)
}

// GPGOption configures a GPGVerifier.
type GPGOption = intverify.GPGOption

// WithAptPinStore injects a persistent apt pin store into the GPG verifier.
func WithAptPinStore(store AptPinStore) GPGOption {
	return intverify.WithAptPinStore(store)
}

// NewHelmProvVerifier constructs the Helm .prov GPG verifier.
func NewHelmProvVerifier(keyringPath string) (*HelmProvVerifier, error) {
	return intverify.NewHelmProvVerifier(keyringPath)
}

// NewGitSignedVerifier constructs the git signed-ref verifier.
func NewGitSignedVerifier(allowedSignersPath, policy string) (*GitSignedVerifier, error) {
	return intverify.NewGitSignedVerifier(allowedSignersPath, policy)
}

// NewCosignVerifier constructs the OCI cosign keyed verifier.
func NewCosignVerifier(cfg CosignConfig, fetcher SignatureFetcher) (*CosignVerifier, error) {
	return intverify.NewCosignVerifier(cfg, fetcher)
}

// NewOCISignatureFetcher discovers cosign signatures via companion tags.
func NewOCISignatureFetcher(registries []string) SignatureFetcher {
	return intverify.NewOCISignatureFetcher(registries)
}

// NewSumDBVerifier constructs the Go module sumdb verifier (TierSigned).
func NewSumDBVerifier(cfg SumDBConfig) *SumDBVerifier {
	return intverify.NewSumDBVerifier(cfg)
}

// NewConsensusVerifier constructs a cross-mirror consensus verifier.
func NewConsensusVerifier(cfg ConsensusConfig, fetcher MirrorDigestFetcher) *ConsensusVerifier {
	return intverify.NewConsensusVerifier(cfg, fetcher)
}

// NewPrivateMatcher builds a GONOSUMDB-style private module matcher.
func NewPrivateMatcher(patterns []string) PrivateMatcher {
	return intverify.NewPrivateMatcher(patterns)
}

// NewHTTPMirrorDigestFetcher returns a metadata-only digest fetcher.
// A non-positive timeout uses the internal default.
func NewHTTPMirrorDigestFetcher(timeout time.Duration) MirrorDigestFetcher {
	return intverify.NewHTTPMirrorDigestFetcher(timeout)
}

// NewMemAptPinStore returns an in-memory apt pin store (tests / demos).
func NewMemAptPinStore() AptPinStore {
	return intverify.NewMemAptPinStore()
}

// SumDBNameFromKey derives the sumdb name advertised to the go command from a
// verifier key (e.g. "sum.golang.org+<hash>+<key>").
func SumDBNameFromKey(vkeyText string) string {
	return intverify.SumDBNameFromKey(vkeyText)
}

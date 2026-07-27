package config

import "github.com/ivanzzeth/specula/internal/ocilayout"

// KnownUpstreamLayouts are valid UpstreamConfig.Layout values.
const (
	UpstreamLayoutHuaweiDDN = ocilayout.HuaweiDDN
)

// ResolveUpstreamPathPrefix returns the OCI path prefix for an upstream.
// PathPrefix wins over Layout. registryHost is the remote registry host
// (e.g. "registry.k8s.io"); empty means Hub → "docker.io" for huawei-ddn.
func ResolveUpstreamPathPrefix(u UpstreamConfig, registryHost string) (string, error) {
	return ocilayout.Resolve(u.PathPrefix, u.Layout, registryHost)
}

// ValidateUpstreamLayout returns an error when Layout is set to an unknown value.
func ValidateUpstreamLayout(u UpstreamConfig) error {
	return ocilayout.ValidateLayout(u.Layout)
}

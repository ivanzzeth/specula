// Package ocilayout resolves OCI upstream path_prefix / layout shorthands.
// Shared by config validation and the OCI handler so expand rules cannot drift.
package ocilayout

import (
	"fmt"
	"strings"
)

// Known Layout values (YAML layout: …).
const (
	HuaweiDDN = "huawei-ddn"
)

// Resolve returns the OCI path segment inserted after /v2/ before the repo name.
// pathPrefix wins over layout. registryHost is the remote registry host
// (e.g. "registry.k8s.io"); empty means Hub → "docker.io" for huawei-ddn.
func Resolve(pathPrefix, layout, registryHost string) (string, error) {
	if p := strings.Trim(strings.TrimSpace(pathPrefix), "/"); p != "" {
		return p, nil
	}
	layout = strings.ToLower(strings.TrimSpace(layout))
	if layout == "" {
		return "", nil
	}
	switch layout {
	case HuaweiDDN:
		host := strings.ToLower(strings.TrimSpace(registryHost))
		if host == "" {
			host = "docker.io"
		}
		return "ddn-k8s/" + host, nil
	default:
		return "", fmt.Errorf("unknown upstream layout %q (want %q)", layout, HuaweiDDN)
	}
}

// ValidateLayout returns an error when layout is set to an unknown value.
func ValidateLayout(layout string) error {
	layout = strings.ToLower(strings.TrimSpace(layout))
	if layout == "" {
		return nil
	}
	if layout != HuaweiDDN {
		return fmt.Errorf("unknown upstream layout %q (want %q)", layout, HuaweiDDN)
	}
	return nil
}

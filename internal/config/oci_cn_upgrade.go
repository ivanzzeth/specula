package config

import (
	"os"
	"strings"
)

// OCIRemoteRegistriesNeedCNUpgrade reports whether the OCI section still looks
// like a pre-CN-multi-mirror install: a lone base_url, a single-upstream chain,
// or a Hub protocol.upstreams list with only one entry. Soft merge leaves those
// forever; install/reinstall should overwrite the oci section from the example.
//
// Operators who intentionally keep a single mirror can set SPECULA_FORCE_CN_OCI=0
// and skip the overwrite path (see ShouldOverwriteOCIOnInstall).
func OCIRemoteRegistriesNeedCNUpgrade(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	pc, ok := cfg.Protocols["oci"]
	if !ok {
		return false
	}
	if len(pc.Upstreams) == 1 {
		return true
	}
	if pc.OCI == nil {
		return false
	}
	for _, rr := range pc.OCI.RemoteRegistries {
		if strings.TrimSpace(rr.BaseURL) != "" && len(rr.Upstreams) == 0 {
			return true
		}
		if len(rr.Upstreams) == 1 {
			return true
		}
	}
	return false
}

// ShouldOverwriteOCIOnInstall decides whether `specula install` should force
// ApplyExample --overwrite --section oci. Default: overwrite when the live
// config looks stale for CN multi-mirror, or when SPECULA_FORCE_CN_OCI=1.
// SPECULA_FORCE_CN_OCI=0 disables the overwrite even when stale.
func ShouldOverwriteOCIOnInstall(cfg *Config) bool {
	switch strings.TrimSpace(os.Getenv("SPECULA_FORCE_CN_OCI")) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return OCIRemoteRegistriesNeedCNUpgrade(cfg)
}

package config

import (
	"net/url"
	"os"
	"strings"
)

// OCIRemoteRegistriesNeedCNUpgrade reports whether the OCI section still looks
// like a pre-CN-multi-mirror install: a lone base_url, a single-upstream chain,
// a Hub protocol.upstreams list with only one entry, or a registry.k8s.io /
// k8s.gcr.io chain that lacks Huawei SWR with a nested path (so large blob
// pulls cannot use the working CN mirror). Soft merge leaves those forever;
// install/reinstall should overwrite the oci section from the example.
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
		host := strings.ToLower(strings.TrimSpace(rr.Host))
		if host == "registry.k8s.io" || host == "k8s.gcr.io" {
			if !remoteRegistryHasNestedK8sMirror(rr) {
				return true
			}
		}
	}
	return false
}

// remoteRegistryHasNestedK8sMirror reports whether the chain is configured for
// a nested (non-transparent) k8s pull path — typically Huawei SWR.
//
// Counts as current when any upstream has:
//   - a non-empty path_prefix (any custom nested layout), or
//   - layout: huawei-ddn on an SWR base_url (swr.*.myhuaweicloud.com)
//
// layout alone on a transparent mirror (DaoCloud/1ms) does not count — that
// would expand to the wrong host path on the wrong registry.
func remoteRegistryHasNestedK8sMirror(rr OCIRemoteRegistry) bool {
	for _, u := range rr.Upstreams {
		if strings.Trim(strings.TrimSpace(u.PathPrefix), "/") != "" {
			return true
		}
		layout := strings.ToLower(strings.TrimSpace(u.Layout))
		if layout == UpstreamLayoutHuaweiDDN && isHuaweiSWRBaseURL(u.BaseURL) {
			return true
		}
	}
	return false
}

func isHuaweiSWRBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasPrefix(host, "swr.") && strings.HasSuffix(host, ".myhuaweicloud.com")
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

package integrate

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/config"
)

type helmRepoSpec struct {
	name string
	path string
}

func helmReposFromConfig(cfg *config.Config) []helmRepoSpec {
	if cfg == nil {
		return defaultHelmRepos()
	}
	proto, ok := cfg.Protocols["helm"]
	if !ok || proto.Helm == nil || len(proto.Helm.Repositories) == 0 {
		return defaultHelmRepos()
	}
	repos := make([]helmRepoSpec, 0, len(proto.Helm.Repositories))
	for _, r := range proto.Helm.Repositories {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		repos = append(repos, helmRepoSpec{
			name: "specula-" + name,
			path: "/helm/" + name,
		})
	}
	if len(repos) == 0 {
		return defaultHelmRepos()
	}
	return repos
}

func defaultHelmRepos() []helmRepoSpec {
	return []helmRepoSpec{
		{name: "specula-bitnami", path: "/helm/bitnami"},
		{name: "specula-prometheus-community", path: "/helm/prometheus-community"},
		{name: "specula-longhorn", path: "/helm/longhorn"},
		{name: "specula-jetstack", path: "/helm/jetstack"},
		{name: "specula-argo", path: "/helm/argo"},
	}
}

func aptArchiveFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return "ubuntu"
	}
	proto, ok := cfg.Protocols["apt"]
	if !ok || proto.Apt == nil || len(proto.Apt.Repositories) == 0 {
		return "ubuntu"
	}
	repos := proto.Apt.Repositories
	for _, r := range repos {
		if strings.TrimSpace(r.Name) == "ubuntu" {
			return "ubuntu"
		}
	}
	name := strings.TrimSpace(repos[0].Name)
	if name == "" {
		return "ubuntu"
	}
	return name
}

// condaChannelNamesFromConfig returns the allowlisted channel NAMES.
//
// Names, not URLs, are what belongs in ~/.condarc's `channels:` list: `conda env
// export` copies that block verbatim into the committed environment.yml, so a
// URL there pins the project to this machine's Specula. The mirror goes in
// `custom_channels` instead (see integrateConda).
func condaChannelNamesFromConfig(cfg *config.Config) []string {
	if cfg == nil {
		return []string{"conda-forge"}
	}
	proto, ok := cfg.Protocols["conda"]
	if !ok || proto.Conda == nil || len(proto.Conda.Channels) == 0 {
		return []string{"conda-forge"}
	}
	names := make([]string, 0, len(proto.Conda.Channels))
	for _, ch := range proto.Conda.Channels {
		if name := strings.TrimSpace(ch.Name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return []string{"conda-forge"}
	}
	return names
}

// NOTE: there is deliberately no condaChannelsFromConfig returning per-channel
// mirror URLs. Those belonged in ~/.condarc's `channels:` list, which
// `conda env export` copies verbatim into the committed environment.yml —
// pinning the project to one machine's Specula. The names go in `channels:`,
// the mirror goes in `custom_channels`.

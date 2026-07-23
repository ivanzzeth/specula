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

func condaChannelsFromConfig(cfg *config.Config, base string) []string {
	base = strings.TrimRight(base, "/")
	if cfg == nil {
		return []string{base + "/conda/conda-forge"}
	}
	proto, ok := cfg.Protocols["conda"]
	if !ok || proto.Conda == nil || len(proto.Conda.Channels) == 0 {
		return []string{base + "/conda/conda-forge"}
	}
	channels := make([]string, 0, len(proto.Conda.Channels))
	for _, ch := range proto.Conda.Channels {
		name := strings.TrimSpace(ch.Name)
		if name == "" {
			continue
		}
		channels = append(channels, base+"/conda/"+name)
	}
	if len(channels) == 0 {
		return []string{base + "/conda/conda-forge"}
	}
	return channels
}

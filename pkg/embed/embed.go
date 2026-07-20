// Package embed mounts Specula protocol handlers onto an http.ServeMux.
//
// Separated from pkg/specula so the core SDK (Get/Open/VerifyFile) does not
// pull every protocol handler (and e.g. go-containerregistry) into the default
// dependency set. Import this package only when embedding HTTP:
//
//	import "github.com/ivanzzeth/specula/pkg/embed"
//	embed.Mount(mux, s, embed.Options{Protocols: []string{"gomod"}})
package embed

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/pkg/handler/apt"
	"github.com/ivanzzeth/specula/pkg/handler/git"
	"github.com/ivanzzeth/specula/pkg/handler/gomod"
	"github.com/ivanzzeth/specula/pkg/handler/helm"
	"github.com/ivanzzeth/specula/pkg/handler/npm"
	"github.com/ivanzzeth/specula/pkg/handler/oci"
	"github.com/ivanzzeth/specula/pkg/handler/pypi"
	"github.com/ivanzzeth/specula/pkg/handler/tarball"
	"github.com/ivanzzeth/specula/pkg/specula"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

// Options configures which protocols to mount and where.
type Options struct {
	// Protocols lists enabled names (oci, gomod, …). Empty = all eight.
	Protocols []string
	// PathPrefix is prepended to every mount (e.g. "/proxy").
	PathPrefix string
	// Upstreams maps protocol → mirrors (passed to handlers that support it).
	Upstreams map[string][]upstream.Upstream
	// QuarantineDir overrides s.QuarantineDir when non-empty.
	QuarantineDir string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Mount registers enabled protocol handlers onto mux using s's cache + meta.
func Mount(mux *http.ServeMux, s *specula.Server, opts Options) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	prefix := strings.TrimRight(opts.PathPrefix, "/")
	qdir := opts.QuarantineDir
	if qdir == "" {
		qdir = s.QuarantineDir()
	}
	ups := opts.Upstreams
	if ups == nil {
		ups = s.Upstreams()
	}
	for _, proto := range enabled(opts.Protocols) {
		h := build(s, proto, qdir, ups, log)
		if h == nil {
			continue
		}
		pattern := patternFor(prefix, proto)
		mux.Handle(pattern, h)
		log.Info("specula/embed: mounted", "protocol", proto, "pattern", pattern)
	}
}

// Handler returns a mux with all enabled protocols mounted.
func Handler(s *specula.Server, opts Options) http.Handler {
	mux := http.NewServeMux()
	Mount(mux, s, opts)
	return mux
}

func enabled(protocols []string) []string {
	if len(protocols) == 0 {
		return []string{"oci", "gomod", "pypi", "npm", "apt", "helm", "tarball", "git"}
	}
	out := make([]string, 0, len(protocols))
	for _, p := range protocols {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "go" {
			p = "gomod"
		}
		out = append(out, p)
	}
	return out
}

func patternFor(prefix, proto string) string {
	mount := map[string]string{
		"oci": "/v2/", "gomod": "/gomod/", "pypi": "/pypi/", "npm": "/npm/",
		"apt": "/apt/", "helm": "/helm/", "tarball": "/tarball/", "git": "/git/",
	}[proto]
	if mount == "" {
		mount = "/" + proto + "/"
	}
	if prefix == "" {
		return mount
	}
	return prefix + mount
}

func build(s *specula.Server, proto, qdir string, ups map[string][]upstream.Upstream, log *slog.Logger) http.Handler {
	list := ups[proto]
	cl := s.UpstreamClient()
	cm := s.CacheManager()
	meta := s.Meta()
	switch proto {
	case "oci":
		opts := []oci.Option{oci.WithQuarantineDir(qdir), oci.WithLogger(log)}
		if len(list) > 0 {
			opts = append(opts, oci.WithUpstream(cl, list))
		}
		return oci.NewHandler(cm, opts...)
	case "gomod":
		opts := []gomod.Option{
			gomod.WithMeta(meta), gomod.WithQuarantineDir(qdir), gomod.WithLogger(log),
			gomod.WithPathPrefix("/gomod"),
		}
		if len(list) > 0 {
			opts = append(opts, gomod.WithUpstream(cl, list))
		}
		return gomod.NewHandler(cm, opts...)
	case "pypi":
		opts := []pypi.Option{pypi.WithQuarantineDir(qdir), pypi.WithLogger(log), pypi.WithPathPrefix("/pypi")}
		if len(list) > 0 {
			opts = append(opts, pypi.WithUpstream(cl, list))
		}
		return pypi.NewHandler(cm, opts...)
	case "npm":
		opts := []npm.Option{npm.WithQuarantineDir(qdir), npm.WithLogger(log), npm.WithPathPrefix("/npm")}
		if len(list) > 0 {
			opts = append(opts, npm.WithUpstream(cl, list))
		}
		return npm.NewHandler(cm, opts...)
	case "apt":
		opts := []apt.Option{apt.WithQuarantineDir(qdir), apt.WithLogger(log), apt.WithPathPrefix("/apt")}
		if len(list) > 0 {
			opts = append(opts, apt.WithUpstream(cl, list))
		}
		return apt.NewHandler(cm, opts...)
	case "helm":
		opts := []helm.Option{helm.WithQuarantineDir(qdir), helm.WithLogger(log), helm.WithPathPrefix("/helm")}
		if len(list) > 0 {
			opts = append(opts, helm.WithUpstream(cl, list))
		}
		return helm.NewHandler(cm, opts...)
	case "tarball":
		return tarball.NewHandler(cm, tarball.WithQuarantineDir(qdir), tarball.WithLogger(log), tarball.WithPathPrefix("/tarball"))
	case "git":
		return git.NewHandler(git.WithLogger(log), git.WithPathPrefix("/git"))
	default:
		return nil
	}
}

// Package webui embeds the Vite build output (web/dist) into the binary
// and serves it as a SPA with proper cache headers.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded dist assets.
// devMode=true injects <script>window.__APP_ENV__="dev"</script> before </head>
// so the frontend can show dev-only UI elements.
func Handler(devMode bool) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embed dist: " + err.Error())
	}
	return newHandler(devMode, sub)
}

// newHandler is the testable core; dist is the rooted sub-FS (index.html at root).
func newHandler(devMode bool, dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))

	rawIndex, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		panic("webui: read dist/index.html: " + err.Error())
	}

	// Pre-compute the index content once; shared across all requests.
	index := rawIndex
	if devMode {
		const devScript = `<script>window.__APP_ENV__="dev"</script>`
		index = bytes.Replace(rawIndex, []byte("</head>"), []byte(devScript+"</head>"), 1)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			serveIndex(w, r, index)
			return
		}
		// Serve real file if it exists; otherwise fall back to index.html (SPA routing).
		if f, err := dist.Open(p); err == nil {
			_ = f.Close()
			// Vite asset filenames contain content hashes → safe for immutable long cache.
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, index)
	})
}

func serveIndex(w http.ResponseWriter, _ *http.Request, index []byte) {
	// index.html must never be long-cached: it references hashed chunk filenames.
	// After a new deploy the old index would reference nonexistent chunks.
	// no-cache forces revalidation on every navigation.
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

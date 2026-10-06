package server

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/c4lyp5o/ezllm/web"
)

// webFS resolves the dashboard source. EZLLM_WEB_DIR (a directory of built
// assets) wins so the UI can be iterated against a running binary without a
// Go rebuild; otherwise the dist/ tree compiled into this binary is served.
func webFS() fs.FS {
	if dir := strings.TrimSpace(os.Getenv("EZLLM_WEB_DIR")); dir != "" {
		return os.DirFS(dir)
	}
	// //go:embed dist keeps the directory in the path (dist/index.html), so root
	// the FS at dist/ — otherwise every lookup misses and the dashboard 404s.
	if sub, err := fs.Sub(web.Dist, "dist"); err == nil {
		return sub
	}
	return web.Dist
}

// spa serves the dashboard on the catch-all "GET /" pattern.
//
// The mux gives every registered API route (/v1/*, /admin/*, /healthz) priority
// over this handler, so the API and the SPA share one origin — which is exactly
// what the unlock gate's copy promises ("The Go binary serves this SPA and the
// /admin API on one origin").
//
// Rules:
//   - existing file            → served, with immutable caching for hashed assets
//   - extensionless unknown    → index.html, so client-side routes
//     (/providers, /combos…) survive a hard refresh
//   - unknown WITH an extension → 404. A missing asset must never be masked by
//     index.html, or a broken deploy would render a blank page instead of failing.
//   - API-shaped paths         → JSON 404 as defence in depth, so a route that is
//     ever removed can never start answering HTML.
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if p == "" || p == "." {
		p = "index.html"
	}
	if p == "admin" || strings.HasPrefix(p, "admin/") ||
		strings.HasPrefix(p, "v1/") || p == "healthz" {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}

	if _, err := fs.Stat(s.web, p); err != nil {
		// index.html missing is a BUILD problem, not a missing asset — say so,
		// otherwise a fresh clone looks like a routing bug.
		if p == "index.html" {
			writeErr(w, http.StatusNotFound, "not_found",
				"dashboard not built: run `npm run build` in web/")
			return
		}
		if path.Ext(p) != "" {
			writeErr(w, http.StatusNotFound, "not_found", "asset not found")
			return
		}
		p = "index.html"
		if _, err := fs.Stat(s.web, p); err != nil {
			writeErr(w, http.StatusNotFound, "not_found",
				"dashboard not built: run `npm run build` in web/")
			return
		}
	}

	switch {
	case strings.HasPrefix(p, "assets/"):
		// vite fingerprints these (index-CctAsTtl.css) → safe to cache forever
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case p == "index.html":
		// must revalidate or a rebuilt dashboard never reaches the browser
		w.Header().Set("Cache-Control", "no-cache")
	}

	r2 := r.Clone(r.Context())
	// FileServer 301s any path ending in /index.html ("./"), which would loop
	// the browser: / → /index.html → /. Hand it "/" for the shell — it serves
	// index.html itself — and the real path for assets.
	if p == "index.html" {
		r2.URL.Path = "/"
	} else {
		r2.URL.Path = "/" + p
	}
	http.FileServer(http.FS(s.web)).ServeHTTP(w, r2)
}

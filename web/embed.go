// Package web embeds the built dashboard so a single `go build` produces a
// binary that serves the SPA and the /admin API on one origin — no node, no
// second process, no file permissions to get wrong on the LAN.
//
// Build order: `npm run build` in web/ FIRST, then `go build ./...`. The dist/
// tree is tracked in git precisely because go:embed needs it present at compile
// time (a fresh clone must build without node). For UI iteration without a Go
// rebuild, point EZLLM_WEB_DIR at a directory and the server serves from disk
// instead of this embed.
package web

import "embed"

// Dist holds web/dist/index.html + assets (hashed filenames → immutable cache).
//
//go:embed all:dist
var Dist embed.FS

// Package web embeds the built frontend SPA into the Go binary.
package web

import (
	"embed"
	"io/fs"
)

// The all: prefix matches the tracked dist/.gitkeep dotfile, so the
// pattern stays valid on a clean checkout with no frontend build.
//go:embed all:dist
var dist embed.FS

// DistFS returns the embedded SPA files, rooted at the dist directory.
func DistFS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

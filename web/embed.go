// Package web embeds HTML templates and static assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Templates returns the embedded template FS.
func Templates() fs.FS { return templatesFS }

// Static returns the embedded static FS rooted at "static".
func Static() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return sub
}

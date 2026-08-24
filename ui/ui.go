// Package ui embeds the watchman's static files (the mark and the page's assets).
package ui

import "embed"

// Static is served under /static/.
//
//go:embed static
var staticFS embed.FS

// Static is the static tree rooted at "static/".
var Static = mustSub(staticFS, "static")

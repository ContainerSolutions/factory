//go:build webui

package webui

import "embed"

// assets holds the built UI from `make web` (internal/webui/dist, gitignored).
//
//go:embed all:dist
var assets embed.FS

const root = "dist"

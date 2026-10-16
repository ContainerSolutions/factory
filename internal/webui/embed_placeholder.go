//go:build !webui

package webui

import "embed"

// assets holds the placeholder page used when the UI hasn't been built,
// so backend builds never need Node. Build with -tags webui to embed the real UI.
//
//go:embed placeholder
var assets embed.FS

const root = "placeholder"

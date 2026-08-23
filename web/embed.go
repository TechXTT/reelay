// Package webui embeds the production Vite build in the Go binary.
package webui

import "embed"

// Dist is served with SPA fallback by internal/api.
//
//go:embed all:dist
var Dist embed.FS

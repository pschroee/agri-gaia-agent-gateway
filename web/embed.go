package web

import "embed"

// Dist contains the built web UI (npm run build creates dist/).
//
//go:embed all:dist
var Dist embed.FS

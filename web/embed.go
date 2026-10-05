package web

import "embed"

// Dist enthält die gebaute Web-UI (npm run build erzeugt dist/).
//
//go:embed all:dist
var Dist embed.FS

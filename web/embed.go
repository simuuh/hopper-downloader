// Package web embeds the built React Web UI (web/dist, produced by `npm
// run build`) into the hopperd binary.
package web

import "embed"

//go:embed all:dist
var DistFS embed.FS

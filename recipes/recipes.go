// Package recipes embeds the per-app recipes; internal/recipes parses them.
package recipes

import "embed"

//go:embed *.yaml
var FS embed.FS

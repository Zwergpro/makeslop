// Package assets embeds static files that are compiled into the makeslop binary.
// Consumers reference the exported variables directly; the binary becomes the
// single source of truth for these files at build time.
package assets

import _ "embed"

//go:embed files/Dockerfile
var Dockerfile []byte

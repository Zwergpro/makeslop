// Package assets embeds static files into the makeslop binary, making the
// binary the single source of truth for them.
package assets

import _ "embed"

//go:embed files/Dockerfile
var Dockerfile []byte

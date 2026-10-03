package main

import (
	"os"

	"github.com/Zwergpro/makeslop/internal/cli"
)

// version is bumped by the release workflow (.github/workflows/release.yaml) so
// `go install` builds report it; release builds also override it via -ldflags.
var version = "v0.4.1"

func main() {
	os.Exit(cli.Main(version, os.Args[1:]))
}

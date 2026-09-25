// Command makeslop is the CLI entry point. Container `exit N` propagates as host `exit N`.
package main

import (
	"os"

	"github.com/Zwergpro/makeslop/internal/cli"
)

// version is bumped by the release workflow (.github/workflows/release.yaml) so
// `go install` builds report it; release builds also override it via -ldflags.
var version = "v0.3.2"

func main() {
	os.Exit(cli.Main(version, os.Args[1:]))
}

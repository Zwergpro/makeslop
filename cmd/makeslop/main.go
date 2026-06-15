// Command makeslop is the CLI entry point. Container `exit N` propagates as host `exit N`.
package main

import (
	"os"

	"github.com/Zwergpro/makeslop/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=…".
var version = "v0.2.1"

func main() {
	os.Exit(cli.Main(version, os.Args[1:]))
}

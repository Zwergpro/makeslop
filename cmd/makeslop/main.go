// Command makeslop is the CLI entry point. Container `exit N` propagates as host `exit N`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// version is set at build time via -ldflags "-X main.version=…"; "dev" otherwise.
var version = "dev"

// newRootCmd constructs the production cobra tree and a cleanup func that closes
// the Docker client (call via defer). docker.New() only parses env vars and
// essentially never fails; if it does, the error is deferred to the first
// docker-touching command via dockerNewErrStub so other commands still work.
func newRootCmd(baseDir string) (*cobra.Command, func()) {
	d, newErr := docker.New()
	if newErr != nil {
		deps := dockerDeps{
			runner:  dockerNewErrStub{newErr},
			builder: dockerNewErrStub{newErr},
			daemon:  dockerNewErrStub{newErr},
			image:   dockerNewErrStub{newErr},
		}
		return newRootCmdWithDeps(baseDir, deps), func() {}
	}
	deps := dockerDeps{runner: d, builder: d, daemon: d, image: d}
	return newRootCmdWithDeps(baseDir, deps), func() { _ = d.Close() }
}

// newRootCmdWithDeps constructs the cobra tree with the given docker deps; used
// by both newRootCmd and tests (which inject fakes).
func newRootCmdWithDeps(baseDir string, deps dockerDeps) *cobra.Command {
	ws := workspace.New(baseDir)

	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based commands with per-workspace cache",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().Bool("quiet", false,
		"suppress stderr chrome (notices, nudges, progress); errors still print")

	rootCmd.AddCommand(
		newInitCmd(ws, baseDir),
		newRunCmd(ws, baseDir, deps),
		newMigrateCmd(baseDir),
		newBuildCmd(baseDir, deps),
		newConfigCmd(baseDir),
		newVersionCmd(),
		newStatusCmd(ws, baseDir, defaultIsTTY, deps),
	)
	return rootCmd
}

// runWithExitCode maps an ExecuteContext() error to a host exit code:
// *docker.ExitError passes through its Code (daemon-reported, e.g. 137 for
// SIGKILL); errSilent -> 1 with no reprint; other errors -> 1 prefixed "makeslop: ".
//
// A SIGINT/SIGTERM-cancellable context is wired into ExecuteContext so every
// subcommand's cmd.Context() is cancellable. contextObserver, when non-nil, is
// called with that context so tests can assert it is not context.Background().
func runWithExitCode(baseDir string, stdout, stderr io.Writer, args []string, contextObserver func(context.Context)) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if contextObserver != nil {
		contextObserver(ctx)
	}

	cmd, closeDocker := newRootCmd(baseDir)
	defer closeDocker()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var de *docker.ExitError
	if errors.As(err, &de) {
		return de.Code
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintf(stderr, "makeslop: %v\n", err)
	}
	return 1
}

func main() {
	baseDir, err := config.DefaultBaseDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		os.Exit(1)
	}
	// Resolve symlinks so docker.Options invariants hold. Missing-dir is fine on
	// first run; other errors (loop, permission) must surface.
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "makeslop: evaluate symlinks for %s: %v\n", baseDir, err)
		os.Exit(1)
	}
	os.Exit(runWithExitCode(baseDir, os.Stdout, os.Stderr, os.Args[1:], nil))
}

package cli

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

// version is set by Main; "dev" otherwise.
var version = "dev"

// Main is the single exported entry point: sets version, resolves baseDir,
// delegates to runWithExitCode.
func Main(v string, args []string) int {
	version = v
	baseDir, err := config.DefaultBaseDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		return 1
	}
	// Missing dir is fine on first run; loop/permission errors must surface.
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "makeslop: evaluate symlinks for %s: %v\n", baseDir, err)
		return 1
	}
	return runWithExitCode(baseDir, os.Stdout, os.Stderr, args, nil)
}

// newRootCmd builds the cobra tree; docker.New() failure is deferred via
// dockerNewErrStub so non-docker commands still work.
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

// newRootCmdWithDeps is the injection point for production and tests.
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
		newLsCmd(baseDir),
		newRemoveCmd(ws),
	)
	return rootCmd
}

// runWithExitCode maps ExecuteContext errors to exit codes: docker.ExitError
// passes Code through; errSilent → 1 no reprint; others → 1 with "makeslop: ".
// contextObserver, when non-nil, is called with the signal-cancellable context
// so tests can assert it is not context.Background().
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
	return exitCodeFromError(cmd.ExecuteContext(ctx), stderr)
}

// exitCodeFromError is the exit-code contract: docker.ExitError passes Code
// through; errSilent → 1 with no reprint; other errors → 1 with "makeslop: ".
func exitCodeFromError(err error, stderr io.Writer) int {
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

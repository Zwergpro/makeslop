// Command makeslop is the CLI entry point: bare `makeslop` launches docker;
// `init` registers the cwd. Container `exit N` propagates as host `exit N`.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// errSilent signals that a RunE has already written a tailored message to
// stderr; main() should exit non-zero without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")

// resolvePwd returns the absolute, EvalSymlinks-resolved cwd required by workspace.Workspaces.
// os.Getwd() already returns an absolute path on POSIX, so no filepath.Abs call is needed.
func resolvePwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("evaluate symlinks for %s: %w", cwd, err)
	}
	return resolved, nil
}

// ensureWithinHome returns errSilent (after printing a hint) when pwd is
// outside the user's home directory and outOfHome is false. pwd must be
// EvalSymlinks-resolved (resolvePwd guarantees this); $HOME is resolved
// here so the comparison is symlink-symmetric.
func ensureWithinHome(stderr io.Writer, pwd string, outOfHome bool) error {
	if outOfHome {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return fmt.Errorf("evaluate symlinks for %s: %w", home, err)
	}
	rel, err := filepath.Rel(resolvedHome, pwd)
	// filepath.Rel returns an error only when both paths are on different
	// Windows volumes; on POSIX (this project's only target) both arguments
	// are always absolute paths on the same volume so err is always nil.
	// Surface it anyway in case the impossible happens.
	if err != nil {
		return fmt.Errorf("compute relative path from %s to %s: %w", resolvedHome, pwd, err)
	}
	if !filepath.IsLocal(rel) {
		fmt.Fprintf(stderr,
			"makeslop: refusing to run from %s (outside %s); pass --out-of-home to override\n",
			pwd, resolvedHome)
		return errSilent
	}
	return nil
}

func newRootCmd(baseDir string) *cobra.Command {
	ws := workspace.New(baseDir)

	var outOfHome bool

	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based commands with per-workspace cache",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pwd, err := resolvePwd()
			if err != nil {
				return err
			}
			if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
				return err
			}
			workspaceRoot, workspaceDir, err := ws.Lookup(pwd)
			if errors.Is(err, workspace.ErrNotRegistered) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"makeslop: no workspace registered for %s; run 'makeslop init' to register it\n",
					pwd)
				return errSilent
			}
			if err != nil {
				return err
			}
			s, err := config.Load(baseDir)
			if err != nil {
				return err
			}
			// ProjectRoot must be the registered ancestor (workspaceRoot), not pwd:
			// running bare makeslop from a subdir must still mount the whole project.
			spec := docker.BuildSpec(docker.Options{
				ProjectRoot:   workspaceRoot,
				WorkspaceName: filepath.Base(workspaceDir),
				BaseDir:       baseDir,
				Image:         s.Image,
				Command:       s.Shell,
			})
			if err := docker.Run(cmd.Context(), spec); err != nil {
				if errors.Is(err, docker.ErrNoTTY) {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"makeslop: stdin/stdout must be a TTY; makeslop is interactive-only")
					return errSilent
				}
				return err
			}
			return nil
		},
	}

	initCmd := &cobra.Command{
		Use:          "init",
		Short:        "Register the current directory as a makeslop workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pwd, err := resolvePwd()
			if err != nil {
				return err
			}
			if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
				return err
			}
			if err := config.Bootstrap(baseDir); err != nil {
				return err
			}
			workspaceDir, err := ws.Init(pwd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
			return nil
		},
	}

	rootCmd.PersistentFlags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")

	rootCmd.AddCommand(initCmd)
	return rootCmd
}

// runWithExitCode maps an Execute() error to a host exit code: *exec.ExitError
// passes through (signal-killed -> 128+signum), errSilent -> 1 with no
// reprint, other errors -> 1 prefixed "makeslop: ".
func runWithExitCode(baseDir string, stdout, stderr io.Writer, args []string) int {
	cmd := newRootCmd(baseDir)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code >= 0 {
			return code
		}
		if wstat, ok := ee.ProcessState.Sys().(syscall.WaitStatus); ok && wstat.Signaled() {
			return 128 + int(wstat.Signal())
		}
		return 255
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
	// Resolve symlinks so docker.Options invariants hold. Missing-dir is fine
	// on first run; other errors (loop, permission) must surface.
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "makeslop: evaluate symlinks for %s: %v\n", baseDir, err)
		os.Exit(1)
	}
	os.Exit(runWithExitCode(baseDir, os.Stdout, os.Stderr, os.Args[1:]))
}

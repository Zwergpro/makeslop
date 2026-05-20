// Command makeslop is the CLI entry point for the makeslop workspace registry.
// Running it bare looks up the cache directory for the current workspace and
// prints the path; `makeslop init` registers the current directory.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/workspace"
)

// errSilent signals that a RunE has already written a tailored message to
// stderr; main() should exit non-zero without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")

// resolvePwd returns the absolute, symlink-resolved cwd — the form
// workspace.Workspaces requires.
func resolvePwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path of %s: %w", cwd, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("evaluate symlinks for %s: %w", abs, err)
	}
	return resolved, nil
}

// newRootCmd builds the cobra tree rooted at `makeslop`, backed by baseDir.
func newRootCmd(baseDir string) *cobra.Command {
	ws := workspace.New(baseDir)

	// SilenceErrors on the root propagates to subcommands, so main() must
	// reprint any non-errSilent error itself.
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
			workspaceDir, err := ws.Lookup(pwd)
			if errors.Is(err, workspace.ErrNotRegistered) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"makeslop: no workspace registered for %s; run 'makeslop init' to register it\n",
					pwd)
				return errSilent
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
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
			workspaceDir, err := ws.Init(pwd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
			return nil
		},
	}

	rootCmd.AddCommand(initCmd)
	return rootCmd
}

func main() {
	baseDir, err := workspace.DefaultBaseDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		os.Exit(1)
	}
	if err := newRootCmd(baseDir).Execute(); err != nil {
		// errSilent => RunE already printed the hint; otherwise reprint here
		// because SilenceErrors is inherited from root.
		if !errors.Is(err, errSilent) {
			fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		}
		os.Exit(1)
	}
}

// Command makeslop is the CLI entry point for the makeslop project cache.
//
// Running it bare (`makeslop`) looks up the cache directory for the current
// working directory's project and prints the path. Running `makeslop init`
// registers the current directory as a project.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/cache"
)

// errSilent wraps an error that has already been reported to stderr by the
// command's RunE. main() recognises it via errors.Is and exits non-zero
// without printing it a second time. Any other non-nil error returned from
// Execute is printed by main() with a "makeslop: " prefix.
var errSilent = errors.New("makeslop: silent error already reported")

// resolvePwd returns the current working directory, normalized to an
// absolute path with symlinks resolved. This is the canonical form that
// cache.Cache expects.
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

// newRootCmd builds the cobra command tree rooted at `makeslop`, wiring it
// to a Cache backed by baseDir. Tests inject a temp baseDir; production
// callers pass cache.DefaultBaseDir().
func newRootCmd(baseDir string) *cobra.Command {
	c := cache.New(baseDir)

	// Note: SilenceErrors is set only on the root command itself, not
	// inherited via PersistentFlags or by intent — cobra's check at command
	// dispatch is `!cmd.SilenceErrors && !c.SilenceErrors` (root). We rely on
	// the errSilent sentinel + main()'s handling to avoid duplicate prints
	// for the not-registered hint, while still ensuring init and other
	// subcommands surface real errors via main().
	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based project commands with per-project cache",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pwd, err := resolvePwd()
			if err != nil {
				return err
			}
			projectDir, err := c.Lookup(pwd)
			if errors.Is(err, cache.ErrNotRegistered) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"makeslop: no project registered for %s; run 'makeslop init' to register it\n",
					pwd)
				return errSilent
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), projectDir)
			return nil
		},
	}

	initCmd := &cobra.Command{
		Use:          "init",
		Short:        "Register the current directory as a makeslop project",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pwd, err := resolvePwd()
			if err != nil {
				return err
			}
			projectDir, err := c.Init(pwd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), projectDir)
			return nil
		},
	}

	rootCmd.AddCommand(initCmd)
	return rootCmd
}

func main() {
	baseDir, err := cache.DefaultBaseDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		os.Exit(1)
	}
	if err := newRootCmd(baseDir).Execute(); err != nil {
		// errSilent => the RunE already wrote a tailored message to stderr.
		// Any other error: surface it ourselves because both root and init
		// inherit SilenceErrors from root, so cobra won't print it.
		if !errors.Is(err, errSilent) {
			fmt.Fprintf(os.Stderr, "makeslop: %v\n", err)
		}
		os.Exit(1)
	}
}

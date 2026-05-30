// Command makeslop is the CLI entry point: bare `makeslop` prints help;
// `go` launches docker; `init` registers the cwd. Container `exit N` propagates as host `exit N`.
package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/networks"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// errSilent signals that a RunE has already written a tailored message to
// stderr; main() should exit non-zero without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")

// resolvePwd returns the absolute, EvalSymlinks-resolved cwd.
// os.Getwd() is already absolute on POSIX, so no filepath.Abs is needed.
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

// ensureWithinHome returns errSilent when pwd is outside the user's home
// directory and outOfHome is false. pwd must be EvalSymlinks-resolved; $HOME
// is resolved here for a symlink-symmetric comparison.
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
	// filepath.Rel never errors on POSIX; surface it anyway.
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

// mergeUniqueSorted returns the sorted union of two string slices. Duplicates
// (across or within the slices) are removed. The inputs are not modified.
func mergeUniqueSorted(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	for _, s := range b {
		seen[s] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// runGo implements the docker-launch logic for the "go" subcommand.
func runGo(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, dryRun bool) error {
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
	masked, err := security.Scan(cmd.Context(), workspaceRoot)
	if errors.Is(err, security.ErrFdMissing) {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"makeslop: fd/fdfind CLI required for secret scanning; install: https://github.com/sharkdp/fd")
		return errSilent
	}
	if err != nil {
		return err
	}
	if len(masked) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "makeslop: masked %d .env file(s)\n", len(masked))
	}
	// YAML parse error aborts launch before docker.Run — symmetric with security.Scan
	// failure to preserve the no-.env-leak invariant.
	yamlExcludes, netCfg, err := projectconfig.Load(workspaceRoot)
	if err != nil {
		return err
	}
	maskedFiles := mergeUniqueSorted(masked, yamlExcludes.Files)

	s, err := config.Load(baseDir)
	if err != nil {
		return err
	}

	// Proxy socket path computed here to keep docker.BuildSpec pure (PID-based, so impure).
	opts := docker.Options{
		ProjectRoot:   workspaceRoot,
		WorkspaceName: filepath.Base(workspaceDir),
		BaseDir:       baseDir,
		Image:         s.Image,
		Command:       s.Shell,
		TmpDirSize:    s.TmpDirSize,
		MaskedFiles:   maskedFiles,
		MaskedDirs:    yamlExcludes.Dirs,
	}

	// sha256(workspaceDir)[:12] + PID: unique across projects and concurrent runs,
	// ~39 bytes (safely under the 108-byte sockaddr_un limit).
	var proxy *networks.Proxy
	if netCfg.ProxyAddress != "" {
		h := sha256.Sum256([]byte(workspaceDir))
		sockPath := filepath.Join("/tmp", fmt.Sprintf("makeslop-%x-%d.sock", h[:6], os.Getpid()))
		opts.ProxySocketHost = sockPath
		opts.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
		proxy = networks.NewProxy(sockPath, netCfg.ProxyAddress)
	}

	// ProjectRoot must be the registered ancestor (workspaceRoot), not pwd:
	// running 'makeslop go' from a subdir must still mount the whole project.
	spec := docker.BuildSpec(opts)

	// Dry-run: print the argv (including proxy plumbing when configured) and
	// return WITHOUT starting the proxy — no socket is bound. The printed argv
	// matches what would execute, satisfying the "printed == executed" invariant.
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

	// Proxy must exist before the container starts; a Start failure aborts the launch.
	if proxy != nil {
		if err := proxy.Start(cmd.Context()); err != nil {
			return err
		}
		defer proxy.Close() //nolint:errcheck // teardown; error not actionable here
	}

	if err := docker.Run(cmd.Context(), spec); err != nil {
		if errors.Is(err, docker.ErrNoTTY) {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"makeslop: stdin/stdout must be a TTY; makeslop is interactive-only")
			return errSilent
		}
		return err
	}
	return nil
}

func newRootCmd(baseDir string) *cobra.Command {
	ws := workspace.New(baseDir)

	var (
		outOfHome bool
		dryRun    bool
	)

	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based commands with per-workspace cache",
		SilenceUsage:  true,
		SilenceErrors: true,
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
			// Init returns the cache dir, not the registered root. Lookup retrieves
			// the root for Scaffold. ErrNotRegistered is impossible — Init just registered pwd.
			workspaceRoot, _, err := ws.Lookup(pwd)
			if err != nil {
				return err
			}
			if err := projectconfig.Scaffold(workspaceRoot); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
			return nil
		},
	}

	goCmd := &cobra.Command{
		Use:          "go",
		Short:        "Launch the docker container for this workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         func(cmd *cobra.Command, _ []string) error { return runGo(cmd, ws, baseDir, outOfHome, dryRun) },
	}

	goCmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
		"print the docker run command instead of executing it")

	migrateCmd := &cobra.Command{
		Use:          "migrate",
		Short:        "Refresh ~/.makeslop with the latest embedded assets",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applied, err := config.Migrate(baseDir)
			if err != nil {
				return err
			}
			if applied {
				fmt.Fprintln(cmd.OutOrStdout(), "makeslop: ~/.makeslop updated")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "makeslop: ~/.makeslop already up to date")
			}
			return nil
		},
	}

	var (
		buildNoCache  bool
		buildBuildArgs []string
	)

	buildCmd := &cobra.Command{
		Use:          "build",
		Short:        "Build (or rebuild) the base docker image from ~/.makeslop/Dockerfile",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.Bootstrap(baseDir); err != nil {
				return err
			}
			s, err := config.Load(baseDir)
			if err != nil {
				return err
			}
			o := docker.BuildOptions{
				Image:          s.Image,
				DockerfilePath: filepath.Join(baseDir, config.DockerfileFile),
				NoCache:        buildNoCache,
				BuildArgs:      buildBuildArgs,
			}
			return docker.Build(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false,
		"do not use cache when building the image")
	buildCmd.Flags().StringArrayVar(&buildBuildArgs, "build-arg", nil,
		"set build-time variables (repeatable)")

	rootCmd.PersistentFlags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")

	rootCmd.AddCommand(initCmd, goCmd, migrateCmd, buildCmd)
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

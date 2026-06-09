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
	"sort"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// version is set at build time via -ldflags "-X main.version=…"; "dev" otherwise.
var version = "dev"

// The four consumer-side docker interfaces. *docker.Docker satisfies all four in
// production; tests inject a fake via newRootCmdWithDeps.
type containerRunner interface {
	Run(ctx context.Context, s docker.Spec) error
}

type imageBuilder interface {
	Build(ctx context.Context, o docker.BuildOptions, out, errw io.Writer) error
}

type daemonChecker interface {
	CheckDaemon(ctx context.Context) error
}

type imageChecker interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

type dockerDeps struct {
	runner  containerRunner
	builder imageBuilder
	daemon  daemonChecker
	image   imageChecker
}

// errSilent signals that a RunE already wrote a tailored message to stderr;
// main() should exit non-zero without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")

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

// ensureWithinHome returns errSilent when pwd is outside the user's home and
// outOfHome is false. pwd must be EvalSymlinks-resolved; $HOME is resolved here
// for a symlink-symmetric comparison.
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
	if err != nil {
		return fmt.Errorf("compute relative path from %s to %s: %w", resolvedHome, pwd, err)
	}
	if !filepath.IsLocal(rel) {
		fmt.Fprintf(stderr,
			"makeslop: refusing to run from %s (outside %s) — pass --out-of-home to override\n",
			pwd, resolvedHome)
		return errSilent
	}
	return nil
}

// mergeUniqueSorted returns the sorted, deduplicated union of two slices.
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

func runRun(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, dryRun, quiet bool, deps dockerDeps) error {
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
			"makeslop: no workspace registered for %s — run 'makeslop init' to register it\n",
			pwd)
		return errSilent
	}
	if err != nil {
		return err
	}
	// YAML parse error aborts before docker.Run — preserves the no-.env-leak invariant.
	yamlExcludes, cacheCfg, envVars, err := projectconfig.Load(workspaceRoot)
	if err != nil {
		return err
	}
	masked, err := security.Scan(cmd.Context(), workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
	if err != nil {
		return err
	}
	if len(masked) > 0 {
		chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
		fmt.Fprintf(chrome, "makeslop: masked %d secret file(s)\n", len(masked))
	}
	maskedFiles := mergeUniqueSorted(masked, yamlExcludes.Files)

	s, err := config.Load(baseDir)
	if err != nil {
		return err
	}

	opts := docker.Options{
		ProjectRoot:       workspaceRoot,
		WorkspaceName:     filepath.Base(workspaceDir),
		BaseDir:           baseDir,
		Image:             s.Image,
		Command:           s.Shell,
		TmpDirSize:        s.TmpDirSize,
		MaskedFiles:       maskedFiles,
		MaskedDirs:        yamlExcludes.Dirs,
		MountContentCache: cacheCfg.Content,
		MountAgentCache:   cacheCfg.Agent,
		Env:               envVars,
	}

	// ProjectRoot is the registered ancestor, not pwd, so running from a subdir
	// still mounts the whole project.
	spec := docker.BuildSpec(opts)

	// Dry-run prints the argv and returns before pre-flight (printed == executed).
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

	// Pre-flight bounded by preflightTimeout so a black-hole DOCKER_HOST cannot hang.
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(cmd.Context())
		daemonErr := deps.daemon.CheckDaemon(pfCtx)
		pfCancel()
		if daemonErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: %v — is docker running?\n", daemonErr)
			return errSilent
		}
	}

	// Image existence: no auto-build, just a remedy.
	var imageFound bool
	var imageErr error
	{
		pfCtx, pfCancel := docker.WithPreflightTimeout(cmd.Context())
		imageFound, imageErr = deps.image.ImageExists(pfCtx, s.Image)
		pfCancel()
	}
	if imageErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: check image %q: %v — is docker running?\n", s.Image, imageErr)
		return errSilent
	}
	if !imageFound {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: image %q not built — run 'makeslop build'\n", s.Image)
		return errSilent
	}

	if err := deps.runner.Run(cmd.Context(), spec); err != nil {
		if errors.Is(err, docker.ErrNoTTY) {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"makeslop: stdin/stdout must be a TTY — run in an interactive terminal")
			return errSilent
		}
		return err
	}
	return nil
}

// quietWriter discards writes when quiet is true; used to gate stderr chrome
// (notices, nudges, progress) while real errors go to the underlying writer.
type quietWriter struct {
	w     io.Writer
	quiet bool
}

func (q *quietWriter) Write(p []byte) (int, error) {
	if q.quiet {
		return len(p), nil
	}
	return q.w.Write(p)
}

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

// dockerNewErrStub returns the docker.New() construction error from every
// operation, so docker-touching commands fail clearly instead of panicking.
type dockerNewErrStub struct{ err error }

func (s dockerNewErrStub) Run(_ context.Context, _ docker.Spec) error { return s.err }
func (s dockerNewErrStub) Build(_ context.Context, _ docker.BuildOptions, _, _ io.Writer) error {
	return s.err
}
func (s dockerNewErrStub) CheckDaemon(_ context.Context) error { return s.err }
func (s dockerNewErrStub) ImageExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}

// newRootCmdWithDeps constructs the cobra tree with the given docker deps; used
// by both newRootCmd and tests (which inject fakes).
func newRootCmdWithDeps(baseDir string, deps dockerDeps) *cobra.Command {
	ws := workspace.New(baseDir)

	var (
		outOfHomeInit bool
		outOfHomeRun  bool
		dryRun        bool
		quiet         bool
		globalOnly    bool
	)

	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based commands with per-workspace cache",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().BoolVar(&quiet, "quiet", false,
		"suppress stderr chrome (notices, nudges, progress); errors still print")

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
			if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHomeInit); err != nil {
				return err
			}

			// Detect fresh vs existing BEFORE Bootstrap to decide whether to stamp
			// MigratedVersion and whether to emit a stale-config nudge.
			freshSeed, err := config.BaseConfigExists(baseDir)
			if err != nil {
				return err
			}
			freshSeed = !freshSeed

			if err := config.Bootstrap(baseDir); err != nil {
				return err
			}
			workspaceDir, err := ws.Init(pwd)
			if err != nil {
				return err
			}
			// Init returns the cache dir; Lookup gives the registered root for Scaffold.
			workspaceRoot, _, err := ws.Lookup(pwd)
			if err != nil {
				return err
			}
			if err := projectconfig.Scaffold(workspaceRoot, projectconfig.Cache{Content: !globalOnly, Agent: !globalOnly}); err != nil {
				return err
			}

			// Fresh seed: stamp MigratedVersion so a newly-init'd dir is never
			// reported stale. ws.Init's lock is already released, so this separate
			// acquisition cannot deadlock.
			if freshSeed {
				if lockErr := config.WithLock(baseDir, func() error {
					s, loadErr := config.Load(baseDir)
					if loadErr != nil {
						return loadErr
					}
					s.MigratedVersion = config.MigrationVersion
					return config.Save(baseDir, s)
				}); lockErr != nil {
					return lockErr
				}
			} else {
				// Existing base config: nudge (non-blocking) if behind. Never stamp
				// MigratedVersion here — that would skip the actual migration.
				s, loadErr := config.Load(baseDir)
				if loadErr != nil {
					return loadErr
				}
				current, latest, stale := config.MigrationStatus(s)
				if stale {
					chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
					fmt.Fprintf(chrome,
						"note: your base config is v%d, latest is v%d — run 'makeslop migrate'\n",
						current, latest)
				}
			}

			// "registered" line is chrome (stderr, gated by --quiet); stdout keeps the bare path.
			chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
			fmt.Fprintf(chrome,
				"registered %s — run 'makeslop build' then 'makeslop run'\n",
				filepath.Base(pwd))
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
			return nil
		},
	}
	// --out-of-home and --global-only are scoped to init only (not root-persistent).
	initCmd.Flags().BoolVar(&outOfHomeInit, "out-of-home", false,
		"allow running outside the user's home directory")
	initCmd.Flags().BoolVar(&globalOnly, "global-only", false,
		"scaffold .makeslop.yaml with project cache overlays disabled (only global ~/.makeslop mounts)")

	runCmd := &cobra.Command{
		Use:          "run",
		Short:        "Launch the docker container for this workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd, ws, baseDir, outOfHomeRun, dryRun, quiet, deps)
		},
	}

	runCmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
		"print the docker run command instead of executing it")
	runCmd.Flags().BoolVar(&outOfHomeRun, "out-of-home", false,
		"allow running outside the user's home directory")

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
		buildNoCache bool
		buildArgs    []string
		buildRefresh bool
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
			if buildRefresh {
				if err := config.WriteDockerfile(baseDir); err != nil {
					return err
				}
				if !quiet {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"makeslop: refreshed ~/.makeslop/Dockerfile from embedded assets")
				}
			}
			s, err := config.Load(baseDir)
			if err != nil {
				return err
			}
			o := docker.BuildOptions{
				Image:          s.Image,
				DockerfilePath: filepath.Join(baseDir, config.DockerfileFile),
				NoCache:        buildNoCache,
				BuildArgs:      buildArgs,
				Quiet:          quiet,
			}
			return deps.builder.Build(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false,
		"do not use cache when building the image")
	buildCmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil,
		"set build-time variables (repeatable)")
	buildCmd.Flags().BoolVar(&buildRefresh, "refresh", false,
		"overwrite ~/.makeslop/Dockerfile from embedded assets before building")

	// Shared by bare `config` and `config list`.
	runConfigList := func(cmd *cobra.Command, _ []string) error {
		s, err := config.Load(baseDir)
		if err != nil {
			return err
		}
		for _, e := range config.ConfigList(s) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", e.Name, e.Value)
		}
		return nil
	}

	configCmd := &cobra.Command{
		Use:          "config",
		Short:        "View or change makeslop settings",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         runConfigList,
	}

	configListCmd := &cobra.Command{
		Use:          "list",
		Short:        "Print current effective settings as key = value lines",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         runConfigList,
	}

	configSetCmd := &cobra.Command{
		Use:          "set <key> <value>",
		Short:        "Validate, set, and persist a configuration key",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pre-validate before locking so a bad input doesn't create the lock
			// file as a side effect. On pre-load failure (corrupt JSON), skip and
			// let the locked path surface the error.
			if scratch, err := config.Load(baseDir); err == nil {
				if err := config.ConfigSet(scratch, args[0], args[1]); err != nil {
					return err
				}
			}

			var storedVal string
			if err := config.WithLock(baseDir, func() error {
				s, err := config.Load(baseDir)
				if err != nil {
					return err
				}
				if err := config.ConfigSet(s, args[0], args[1]); err != nil {
					return err
				}
				if err := config.Save(baseDir, s); err != nil {
					return err
				}
				// Echo the stored (normalized) value, not the raw argument.
				storedVal, _ = config.ConfigGet(s, args[0])
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", args[0], storedVal)
			return nil
		},
	}

	configCmd.AddCommand(configListCmd, configSetCmd)

	versionCmd := &cobra.Command{
		Use:          "version",
		Short:        "Print the makeslop version and exit",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}

	statusCmd := newStatusCmd(ws, baseDir, defaultIsTTY, deps)

	rootCmd.AddCommand(initCmd, runCmd, migrateCmd, buildCmd, configCmd, versionCmd, statusCmd)
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

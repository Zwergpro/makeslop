// Command makeslop is the CLI entry point: bare `makeslop` prints help;
// `run` launches docker; `init` registers the cwd. Container `exit N` propagates as host `exit N`.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// version is set at build time via ldflags:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// The default "dev" value is used when the binary is built without ldflags.
var version = "dev"

// validHostRe is a strict allowlist for the host portion of a proxy address.
// It permits hostnames, IPv4 addresses, and bracketed IPv6 (net.SplitHostPort
// strips the brackets, so the host seen here is already unwrapped).
var validHostRe = regexp.MustCompile(`^[a-zA-Z0-9.\-\[\]:]+$`)

// sidecarRunner is the interface used by runRun for the socat sidecar lifecycle.
// Satisfied by *docker.Sidecar; tests swap newSidecarFn to return a fake.
type sidecarRunner interface {
	Start(ctx context.Context, upstream string, volumeName string) error
	Close() error
}

// newSidecarFn is the seam used by tests to capture or replace Sidecar creation.
// The default returns a real *docker.Sidecar; tests swap it via setSidecarFnForTest.
var newSidecarFn = func(quiet bool, stderr io.Writer) sidecarRunner {
	return docker.NewSidecar(quiet, stderr)
}

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
			"makeslop: refusing to run from %s (outside %s) — pass --out-of-home to override\n",
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

// runRun implements the docker-launch logic for the "run" subcommand.
//
// Two-state network model:
//
//   - Direct (default): no --proxy flag and no network.proxy.address → bridge
//     networking; no socat sidecar, no volume, no --network none.
//   - Proxy: --proxy ip:port OR network.proxy.address (flag wins) → --network none;
//     egress = volume unix socket → socat sidecar → TCP-CONNECT:<ip>:<port> → remote
//     HTTP proxy. The app is airtight: apps that ignore HTTP_PROXY cannot leak.
//
// The host-side Gateway (internal/networks) is gone; socat connects directly to the
// remote upstream over bridge networking, so no host process is required.
func runRun(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, dryRun, quiet bool, proxyFlag string) error {
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
	// YAML parse error aborts launch before docker.Run — symmetric with security.Scan
	// failure to preserve the no-.env-leak invariant.
	yamlExcludes, netCfg, cacheCfg, err := projectconfig.Load(workspaceRoot)
	if err != nil {
		return err
	}
	masked, err := security.Scan(cmd.Context(), workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
	if err != nil {
		return err
	}
	if len(masked) > 0 {
		// "masked N" is stderr chrome — gated by --quiet.
		chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
		fmt.Fprintf(chrome, "makeslop: masked %d secret file(s)\n", len(masked))
	}
	maskedFiles := mergeUniqueSorted(masked, yamlExcludes.Files)

	s, err := config.Load(baseDir)
	if err != nil {
		return err
	}

	// Resolve the effective proxy upstream: --proxy flag wins over network.proxy.address.
	// When both are empty, direct/bridge networking is used (no socat, no volume).
	upstream := proxyFlag
	if upstream == "" {
		upstream = netCfg.ProxyAddress
	}
	// Fail loud for a malformed upstream address before daemon pre-flight checks.
	if upstream != "" {
		h, p, err := net.SplitHostPort(upstream)
		if err != nil || h == "" || p == "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: invalid proxy address %q — must be host:port\n", upstream)
			return errSilent
		}
		if !validHostRe.MatchString(h) {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: invalid proxy address %q — host contains illegal characters\n", upstream)
			return errSilent
		}
		portNum, atoiErr := strconv.Atoi(p)
		if atoiErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: invalid proxy address %q — port must be a plain integer\n", upstream)
			return errSilent
		}
		if portNum < 1 || portNum > 65535 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: invalid proxy address %q — port must be in range [1, 65535]\n", upstream)
			return errSilent
		}
	}

	// Proxy socket path computed here to keep docker.BuildSpec pure (PID-based, so impure).
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
	}

	// sha256(workspaceDir)[:12] + PID: unique volume name across projects and concurrent runs.
	// Only set when proxy mode is active (upstream != "").
	var volumeName string
	if upstream != "" {
		h := sha256.Sum256([]byte(workspaceDir))
		volumeName = fmt.Sprintf("makeslop-sock-%x-%d", h[:6], os.Getpid())
		opts.ProxySocketVolume = volumeName
	}

	// ProjectRoot must be the registered ancestor (workspaceRoot), not pwd:
	// running 'makeslop run' from a subdir must still mount the whole project.
	spec := docker.BuildSpec(opts)

	// Dry-run: print the argv (including proxy plumbing when configured) and
	// return WITHOUT running pre-flight or starting the proxy — no socket is bound.
	// The printed argv matches what would execute, satisfying the "printed == executed" invariant.
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

	// Pre-flight: daemon reachability. Must happen after workspace/config resolution
	// (so we have the image name) but before proxy start.
	if err := docker.CheckDaemon(cmd.Context()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: %v — is docker running?\n", err)
		return errSilent
	}

	// Pre-flight: image existence. No auto-build; a clear remedy is provided.
	imageFound, imageErr := docker.ImageExists(cmd.Context(), s.Image)
	if imageErr != nil {
		return imageErr
	}
	if !imageFound {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: image %q not built — run 'makeslop build'\n", s.Image)
		return errSilent
	}

	// Proxy mode: start the socat sidecar that bridges the volume unix socket to
	// the remote upstream TCP address. Any Start failure aborts the launch
	// (fail-loud invariant). Teardown via defer.
	if upstream != "" {
		// --quiet gates the "pulling socat image" notice.
		sc := newSidecarFn(quiet, cmd.ErrOrStderr())
		if err := sc.Start(cmd.Context(), upstream, volumeName); err != nil {
			return err
		}
		defer sc.Close() //nolint:errcheck // teardown; error not actionable here
	}

	if err := docker.Run(cmd.Context(), spec); err != nil {
		if errors.Is(err, docker.ErrNoTTY) {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"makeslop: stdin/stdout must be a TTY — run in an interactive terminal")
			return errSilent
		}
		return err
	}
	return nil
}

// quietWriter wraps an io.Writer and discards writes when quiet is true.
// It is used to gate stderr chrome (notices, nudges, progress) while letting
// callers that write to the underlying writer (e.g. cobra's SetErr) use the
// real writer for actual errors.
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

func newRootCmd(baseDir string) *cobra.Command {
	ws := workspace.New(baseDir)

	var (
		outOfHomeInit bool
		outOfHomeRun  bool
		dryRun        bool
		quiet         bool
		proxyFlag     string
		globalOnly    bool
	)

	rootCmd := &cobra.Command{
		Use:           "makeslop",
		Short:         "Run docker-based commands with per-workspace cache",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// --quiet is persistent so all subcommands inherit it.
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

			// Record fresh vs existing BEFORE Bootstrap so we can decide whether
			// to stamp MigratedVersion and whether to emit a stale-config nudge.
			freshSeed, err := config.BaseConfigExists(baseDir)
			if err != nil {
				return err
			}
			freshSeed = !freshSeed // BaseConfigExists returns true when present; we want "absent = fresh"

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
			if err := projectconfig.Scaffold(workspaceRoot, projectconfig.Cache{Content: !globalOnly, Agent: !globalOnly}); err != nil {
				return err
			}

			// Fresh seed: stamp MigratedVersion so a newly-init'd dir is never
			// reported stale. ws.Init already called Save once; reload and re-save
			// with the stamp so we don't race against any field changes.
			if freshSeed {
				s, loadErr := config.Load(baseDir)
				if loadErr != nil {
					return loadErr
				}
				s.MigratedVersion = config.MigrationVersion
				if saveErr := config.Save(baseDir, s); saveErr != nil {
					return saveErr
				}
			} else {
				// Existing base config: check for staleness and nudge if behind.
				s, loadErr := config.Load(baseDir)
				if loadErr != nil {
					return loadErr
				}
				current, latest, stale := config.MigrationStatus(s)
				if stale {
					// Nudge is chrome (non-blocking), gated by --quiet.
					chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
					fmt.Fprintf(chrome,
						"note: your base config is v%d, latest is v%d — run 'makeslop migrate'\n",
						current, latest)
				}
			}

			// Success: stderr chrome is gated by --quiet; stdout keeps the bare path.
			// The "registered" line is human notice (chrome), so route through quietWriter.
			chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
			fmt.Fprintf(chrome,
				"registered %s — run 'makeslop build' then 'makeslop run'\n",
				filepath.Base(pwd))
			fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
			return nil
		},
	}
	// --out-of-home is scoped to init only (not a persistent flag on root).
	initCmd.Flags().BoolVar(&outOfHomeInit, "out-of-home", false,
		"allow running outside the user's home directory")
	// --global-only is scoped to init only (not a persistent flag on root).
	// Scaffolds .makeslop.yaml with both cache groups disabled so only the global
	// ~/.makeslop mounts are active. No-op when .makeslop.yaml already exists
	// (Scaffold is idempotent).
	initCmd.Flags().BoolVar(&globalOnly, "global-only", false,
		"scaffold .makeslop.yaml with project cache overlays disabled (only global ~/.makeslop mounts)")

	runCmd := &cobra.Command{
		Use:          "run",
		Short:        "Launch the docker container for this workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd, ws, baseDir, outOfHomeRun, dryRun, quiet, proxyFlag)
		},
	}

	runCmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
		"print the docker run command instead of executing it")
	// --out-of-home is scoped to run only (not a persistent flag on root).
	runCmd.Flags().BoolVar(&outOfHomeRun, "out-of-home", false,
		"allow running outside the user's home directory")
	// --proxy is scoped to run only (not a persistent flag on root).
	// Enables proxy mode: routes container egress through a remote HTTP proxy.
	// Flag wins over network.proxy.address. Rejected as unknown by other commands.
	runCmd.Flags().StringVar(&proxyFlag, "proxy", "",
		"route container egress through a remote HTTP proxy (host:port)")

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
			}
			return docker.Build(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false,
		"do not use cache when building the image")
	buildCmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil,
		"set build-time variables (repeatable)")
	buildCmd.Flags().BoolVar(&buildRefresh, "refresh", false,
		"overwrite ~/.makeslop/Dockerfile from embedded assets before building")

	// runConfigList is the shared implementation for `config` (bare) and
	// `config list` — both print key = value lines from ConfigList.
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
			// Echo the stored value (not the raw CLI argument) so the output
			// reflects what ConfigList would show (e.g. whitespace is trimmed).
			// ConfigSet only succeeds for registered keys, so ConfigGet is
			// guaranteed to find the entry.
			storedVal, _ := config.ConfigGet(s, args[0])
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

	statusCmd := newStatusCmd(ws, baseDir, defaultIsTTY)

	rootCmd.AddCommand(initCmd, runCmd, migrateCmd, buildCmd, configCmd, versionCmd, statusCmd)
	return rootCmd
}

// runWithExitCode maps an Execute() error to a host exit code:
//   - *docker.ExitError passes through its Code (the daemon-reported exit status,
//     e.g. 137 for SIGKILL); this is the primary path for `makeslop run`.
//   - errSilent -> 1 with no reprint.
//   - other errors -> 1 prefixed "makeslop: ".
func runWithExitCode(baseDir string, stdout, stderr io.Writer, args []string) int {
	cmd := newRootCmd(baseDir)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
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

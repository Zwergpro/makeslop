package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// filterOut returns a copy of s with the element equal to exclude removed.
// Returns s unmodified (no copy) when exclude is not present.
func filterOut(s []string, exclude string) []string {
	for i, v := range s {
		if v == exclude {
			out := make([]string, 0, len(s)-1)
			out = append(out, s[:i]...)
			out = append(out, s[i+1:]...)
			return out
		}
	}
	return s
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

// sandboxMountGates inspects the filesystem state at workspaceRoot to decide
// which sandbox-policy mounts BuildSpec should activate. The returned filtered
// slice is maskedFiles with the .makeslop.yaml entry removed when protect is
// true (prevents a /dev/null bind from overriding the read-only self-bind).
//
// protect — true iff .makeslop.yaml is a regular file (missing or non-regular
//
//	→ the bind source would be absent or wrong type, failing container create).
//
// maskHooks — true iff .git is a directory (gitfile/worktree/submodule → false;
//
//	the real hooks dir lives outside the workspace, documented residual risk).
func sandboxMountGates(workspaceRoot string, maskedFiles []string) (protect, maskHooks bool, filtered []string) {
	configPath := filepath.Join(workspaceRoot, projectconfig.Filename)
	if fi, err := os.Lstat(configPath); err == nil {
		// Only bind-mount when it is a regular file: a missing bind source fails
		// container create, and masking an absent file would create an empty one
		// through the rw bind (disabling scan patterns).
		protect = fi.Mode().IsRegular()
	}

	// When ProtectProjectConfig is active, drop .makeslop.yaml from the /dev/null
	// masked-file list if the scan or an explicit exclude.files entry found it.
	// Docker applies mounts in order; last-write-wins, so a /dev/null bind appended
	// after the read-only self-bind would silently override it (e.g. when the user
	// adds a broad pattern like "*.yaml" to their exclude.scan.patterns).
	// Note: no equivalent guard is needed for yamlExcludes.Dirs — reservedPaths in
	// projectconfig rejects ".makeslop.yaml" from exclude.dirs at parse time, so it
	// can never appear there.
	filtered = maskedFiles
	if protect {
		filtered = filterOut(maskedFiles, configPath)
	}

	if fi, err := os.Lstat(filepath.Join(workspaceRoot, ".git")); err == nil {
		// Only overlay hooks when .git is a directory. In git worktrees/submodules
		// .git is a regular file (gitfile); the real hooks dir lives outside the
		// workspace and is therefore not masked (documented residual risk).
		maskHooks = fi.IsDir()
	}
	return protect, maskHooks, filtered
}

// reportScanResults prints the "masked N secret file(s)" chrome (gated by
// --quiet via chrome) and symlink warnings (always to stderr, bypassing --quiet).
// root is used to compute workspace-relative paths for the symlink messages;
// on filepath.Rel failure the absolute path is printed.
func reportScanResults(stderr, chrome io.Writer, root string, masked, symlinkMatches []string) {
	if len(masked) > 0 {
		fmt.Fprintf(chrome, "makeslop: masked %d secret file(s)\n", len(masked))
	}
	for _, sym := range symlinkMatches {
		rel, relErr := filepath.Rel(root, sym)
		if relErr != nil {
			rel = sym
		}
		fmt.Fprintf(stderr,
			"makeslop: warning: symlink %s matches a secret pattern but is NOT masked\n", rel)
	}
}

// runRun implements the run command body. It is a named function so it can be
// called from the runCmd RunE closure, keeping the closure thin.
func runRun(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, dryRun, quiet bool, deps dockerDeps) error {
	chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
	pwd, err := resolvePwd()
	if err != nil {
		return err
	}
	if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
		return err
	}

	// Load settings once at the top. ws.Lookup uses the already-loaded settings
	// rather than loading a second copy — avoids a redundant read and any
	// inconsistency between two independent loads.
	s, err := config.Load(baseDir)
	if err != nil {
		return err
	}

	workspaceRoot, workspaceDir, err := ws.Lookup(s, pwd)
	if errors.Is(err, workspace.ErrNotRegistered) {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: no workspace registered for %s — run 'makeslop init' to register it\n",
			pwd)
		return errSilent
	}
	if err != nil {
		return err
	}

	// Daemon preflight directly after workspace lookup, before the secret scan.
	// Skipped on --dry-run so dry-run continues to work without a live daemon.
	// This avoids making the user wait through a potentially long scan when the
	// daemon is simply not running.
	if !dryRun {
		if daemonErr := deps.checkDaemonPreflight(cmd.Context()); daemonErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: %v — is docker running?\n", daemonErr)
			return errSilent
		}
	}

	// YAML parse error aborts before docker.Run — preserves the no-.env-leak invariant.
	yamlExcludes, cacheCfg, envVars, err := projectconfig.Load(workspaceRoot)
	if err != nil {
		return err
	}

	// Print projectconfig symlink warnings directly to stderr, bypassing --quiet.
	// Degraded protection is always surfaced regardless of verbosity.
	for _, w := range yamlExcludes.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "makeslop: warning: %s\n", w)
	}

	masked, symlinkMatches, err := security.Scan(cmd.Context(), workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
	if err != nil {
		return err
	}
	// "masked N" is chrome (quiet-gated); symlink warnings bypass --quiet.
	reportScanResults(cmd.ErrOrStderr(), chrome, workspaceRoot, masked, symlinkMatches)
	maskedFiles := mergeUniqueSorted(masked, yamlExcludes.Files)

	// Gate sandbox-policy mounts on host filesystem state. BuildSpec is pure
	// (no filesystem access), so the Lstat checks live here.
	protectProjectConfig, maskGitHooks, maskedFiles := sandboxMountGates(workspaceRoot, maskedFiles)

	opts := docker.Options{
		ProjectRoot:          workspaceRoot,
		WorkspaceName:        filepath.Base(workspaceDir),
		WorkspaceHost:        workspaceDir,
		BaseDir:              baseDir,
		Image:                s.Image,
		Command:              s.Shell,
		TmpDirSize:           s.TmpDirSize,
		MaskedFiles:          maskedFiles,
		MaskedDirs:           yamlExcludes.Dirs,
		MountContentCache:    cacheCfg.Content,
		MountAgentCache:      cacheCfg.Agent,
		Env:                  envVars,
		ProtectProjectConfig: protectProjectConfig,
		MaskGitHooks:         maskGitHooks,
	}

	// ProjectRoot is the registered ancestor, not pwd, so running from a subdir
	// still mounts the whole project.
	spec := docker.BuildSpec(opts)

	// Dry-run prints the argv and returns before image pre-flight (printed == executed).
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

	// Image existence: no auto-build, just a remedy.
	imageFound, imageErr := deps.imageExistsPreflight(cmd.Context(), s.Image)
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

// newRunCmd constructs the "run" cobra.Command, wiring flags and RunE to runRun.
func newRunCmd(ws *workspace.Workspaces, baseDir string, deps dockerDeps) *cobra.Command {
	var outOfHome bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:          "run",
		Short:        "Launch the docker container for this workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			return runRun(cmd, ws, baseDir, outOfHome, dryRun, quiet, deps)
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
		"print the docker run command instead of executing it")
	cmd.Flags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")
	return cmd
}

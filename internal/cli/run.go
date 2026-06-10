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

// removes only the first occurrence; noop when exclude is absent
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

// returns nil (not empty slice) when both inputs are empty
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

// sandboxMountGates resolves filesystem state at workspaceRoot and returns
// the two sandbox-policy flags plus the filtered masked-files list.
func sandboxMountGates(workspaceRoot string, maskedFiles []string) (protect, maskHooks bool, filtered []string) {
	configPath := filepath.Join(workspaceRoot, projectconfig.Filename)
	if fi, err := os.Lstat(configPath); err == nil {
		// Regular file only: a missing bind source fails container create.
		protect = fi.Mode().IsRegular()
	}

	// Drop .makeslop.yaml from the /dev/null masked list when protect is active.
	// Docker applies mounts last-write-wins, so a /dev/null bind after the
	// read-only self-bind would silently override it.
	filtered = maskedFiles
	if protect {
		filtered = filterOut(maskedFiles, configPath)
	}

	if fi, err := os.Lstat(filepath.Join(workspaceRoot, ".git")); err == nil {
		// Gitfile/worktree/submodule: .git is a regular file; real hooks dir is
		// outside workspace (documented residual risk) — only overlay on a dir.
		maskHooks = fi.IsDir()
	}
	return protect, maskHooks, filtered
}

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

func runRun(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, dryRun, quiet bool, deps dockerDeps) error {
	chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}
	pwd, err := resolvePwd()
	if err != nil {
		return err
	}
	if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
		return err
	}

	// Load once; pass the same *Settings to ws.Lookup to avoid a redundant read.
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

	// Before the scan: a down daemon is reported immediately. Skipped on --dry-run.
	if !dryRun {
		if daemonErr := deps.checkDaemonPreflight(cmd.Context()); daemonErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"makeslop: %v — is docker running?\n", daemonErr)
			return errSilent
		}
	}

	yamlExcludes, cacheCfg, envVars, err := projectconfig.Load(workspaceRoot)
	if err != nil {
		return err
	}

	// Symlink warnings bypass --quiet: degraded protection is never treated as chrome.
	for _, w := range yamlExcludes.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "makeslop: warning: %s\n", w)
	}

	masked, symlinkMatches, err := security.Scan(cmd.Context(), workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)
	if err != nil {
		return err
	}
	reportScanResults(cmd.ErrOrStderr(), chrome, workspaceRoot, masked, symlinkMatches)
	maskedFiles := mergeUniqueSorted(masked, yamlExcludes.Files)

	// BuildSpec is pure (no fs access); Lstat checks live here.
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

	spec := docker.BuildSpec(opts)

	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

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

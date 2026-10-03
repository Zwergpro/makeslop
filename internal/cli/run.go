package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

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

// resolveEnv decides the final -e order after host lookup. Missing host names
// are skipped; present values, including empty ones, pass through unchanged.
func resolveEnv(env projectconfig.Env, lookup func(string) (string, bool)) []string {
	out := make([]string, 0, len(env.Static)+len(env.Host))
	out = append(out, env.Static...)
	for _, name := range env.Host {
		if v, ok := lookup(name); ok {
			out = append(out, name+"="+v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	// projectconfig rejects duplicate keys, making an unstable sort safe.
	sort.Slice(out, func(i, j int) bool {
		ki, _, _ := strings.Cut(out[i], "=")
		kj, _, _ := strings.Cut(out[j], "=")
		return ki < kj
	})
	return out
}

// sandboxMountGates checks live paths before the pure BuildSpec call.
func sandboxMountGates(workspaceRoot string) (protect, maskHooks bool) {
	configPath := filepath.Join(workspaceRoot, projectconfig.Filename)
	if fi, err := os.Lstat(configPath); err == nil {
		// A missing bind source would fail container creation.
		protect = fi.Mode().IsRegular()
	}

	if fi, err := os.Lstat(filepath.Join(workspaceRoot, ".git")); err == nil {
		// Gitfile/worktree/submodule: .git is a regular file; real hooks dir is
		// outside workspace (documented residual risk) — only overlay on a dir.
		maskHooks = fi.IsDir()
	}
	return protect, maskHooks
}

// Symlink warnings bypass quietWriter because they signal incomplete masking.
func reportScanResults(stderr, chrome io.Writer, root string, join bool, masked, symlinkMatches []string) {
	prefix, in := "", ""
	if join {
		prefix, in = "join "+root+": ", " in "+root
	}
	if len(masked) > 0 {
		fmt.Fprintf(chrome, "makeslop: masked %d secret file(s)%s\n", len(masked), in)
	}
	for _, sym := range symlinkMatches {
		rel, relErr := filepath.Rel(root, sym)
		if relErr != nil {
			rel = sym
		}
		fmt.Fprintf(stderr,
			"makeslop: warning: %ssymlink %s matches a secret pattern but is NOT masked\n", prefix, rel)
	}
}

// Requiring a join's config again closes the gap between path validation and
// loading: removal cannot silently disable its masks.
func loadProject(ctx context.Context, stderr, chrome io.Writer, root string, join bool) (docker.Project, projectconfig.Config, error) {
	load, prefix := projectconfig.Load, ""
	if join {
		load, prefix = projectconfig.LoadExisting, "join "+root+": "
	}

	pcfg, err := load(root)
	if join && errors.Is(err, fs.ErrNotExist) {
		return docker.Project{}, projectconfig.Config{}, errNotProject
	}
	if err != nil {
		return docker.Project{}, projectconfig.Config{}, err
	}

	for _, w := range pcfg.Excludes.Warnings {
		fmt.Fprintf(stderr, "makeslop: warning: %s%s\n", prefix, w)
	}

	// Cache has no presence marker, so only observable ignored settings get a notice.
	if join && (len(pcfg.Env.Static) > 0 || len(pcfg.Env.Host) > 0 ||
		pcfg.Network.Mode != "" || len(pcfg.Network.Networks) > 0) {
		fmt.Fprintf(chrome, "makeslop: %senvironments/network settings ignored\n", prefix)
	}

	masked, symlinkMatches, err := security.Scan(ctx, root, pcfg.Excludes.Patterns, pcfg.Excludes.SkipDirs)
	if err != nil {
		return docker.Project{}, projectconfig.Config{}, err
	}
	reportScanResults(stderr, chrome, root, join, masked, symlinkMatches)

	protect, maskHooks := sandboxMountGates(root)

	return docker.Project{
		Host:          root,
		MaskedFiles:   mergeUniqueSorted(masked, pcfg.Excludes.Files),
		MaskedDirs:    pcfg.Excludes.Dirs,
		ProtectConfig: protect,
		MaskGitHooks:  maskHooks,
	}, pcfg, nil
}

func runRun(cmd *cobra.Command, ws *workspace.Workspaces, baseDir, imageFlag string, joinFlags []string, outOfHome, dryRun, quiet bool, deps dockerDeps) error {
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

	// Resolve the image first so an unregistered workspace cannot hide a
	// missing image setting.
	image, err := resolveImage(imageFlag, s.Image)
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

	// Path checks only; join configs are parsed after the daemon preflight.
	mainName := filepath.Base(workspaceDir)
	joins, err := resolveJoins(pwd, workspaceRoot, mainName, baseDir, joinFlags, outOfHome)
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

	mainProj, pcfg, err := loadProject(cmd.Context(), cmd.ErrOrStderr(), chrome, workspaceRoot, false)
	if err != nil {
		return err
	}
	mainProj.Name = mainName
	mainProj.Label = "project: " + workspaceRoot
	projects := []docker.Project{mainProj}

	for _, j := range joins {
		p, _, err := loadProject(cmd.Context(), cmd.ErrOrStderr(), chrome, j.Host, true)
		if err != nil {
			return fmt.Errorf("join %s: %w", j.Host, err)
		}
		p.Name = j.Name
		p.ReadOnly = j.ReadOnly
		p.Label = j.label()
		projects = append(projects, p)
	}

	opts := docker.Options{
		Projects:          projects,
		WorkspaceHost:     workspaceDir,
		BaseDir:           baseDir,
		Image:             image,
		Command:           s.Shell,
		TmpDirSize:        s.TmpDirSize,
		MountContentCache: pcfg.Cache.Content,
		MountAgentCache:   pcfg.Cache.Agent,
		Env:               resolveEnv(pcfg.Env, os.LookupEnv),
		NetworkMode:       pcfg.Network.Mode,
		Networks:          pcfg.Network.Networks,
	}

	spec := docker.BuildSpec(opts)

	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), spec.ShellCommand())
		return nil
	}

	imageFound, imageErr := deps.imageExistsPreflight(cmd.Context(), image)
	if imageErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"makeslop: check image %q: %v — is docker running?\n", image, imageErr)
		return errSilent
	}
	if !imageFound {
		fmt.Fprintln(cmd.ErrOrStderr(), "makeslop: "+imageNotFoundHint(image))
		return errSilent
	}

	if err := deps.networkPreflight(cmd.Context(), pcfg.Network); err != nil {
		return err
	}

	if err := deps.api.Run(cmd.Context(), spec); err != nil {
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
	var image string
	var joins []string

	cmd := &cobra.Command{
		Use:          "run",
		Short:        "Launch the docker container for this workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			return runRun(cmd, ws, baseDir, image, joins, outOfHome, dryRun, quiet, deps)
		},
	}
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
		"print the docker run command instead of executing it")
	cmd.Flags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")
	cmd.Flags().StringVarP(&image, "image", "i", "",
		"container image to run (overrides the settings image)")
	cmd.Flags().StringArrayVarP(&joins, "join", "j", nil,
		"mount another makeslop project at /workspace/<basename>; `path[:ro|:rw]`, repeatable (default rw)")
	return cmd
}

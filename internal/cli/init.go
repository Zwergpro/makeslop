package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// stampMigratedVersion atomically loads, stamps MigratedVersion = MigrationVersion,
// and saves settings. Called only on a fresh seed (no prior settings.json). The
// ws.Init lock is already released before this is called, so the separate
// WithLock acquisition cannot deadlock.
func stampMigratedVersion(baseDir string) error {
	return config.WithLock(baseDir, func() error {
		s, err := config.Load(baseDir)
		if err != nil {
			return err
		}
		s.MigratedVersion = config.MigrationVersion
		return config.Save(baseDir, s)
	})
}

// runInit implements the init command body. It is a named function so it can be
// called from the initCmd RunE closure, keeping the closure thin.
func runInit(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, globalOnly, quiet bool) error {
	chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}

	pwd, err := resolvePwd()
	if err != nil {
		return err
	}
	if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
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

	// Load settings once after ws.Init; reuse for both Lookup (to find the
	// registered root for Scaffold) and the stale-nudge check below.
	// ws.Init's lock is already released, so this Load does not deadlock.
	initSettings, loadErr := config.Load(baseDir)
	if loadErr != nil {
		return loadErr
	}

	// Lookup gives the registered root for Scaffold.
	workspaceRoot, _, err := ws.Lookup(initSettings, pwd)
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
		if lockErr := stampMigratedVersion(baseDir); lockErr != nil {
			return lockErr
		}
	} else {
		// Existing base config: nudge (non-blocking) if behind. Never stamp
		// MigratedVersion here — that would skip the actual migration.
		// Reuse initSettings from the load above.
		current, latest, stale := config.MigrationStatus(initSettings)
		if stale {
			fmt.Fprintf(chrome,
				"note: your base config is v%d, latest is v%d — run 'makeslop migrate'\n",
				current, latest)
		}
	}

	// "registered" line is chrome (stderr, gated by --quiet); stdout keeps the bare path.
	fmt.Fprintf(chrome,
		"registered %s — run 'makeslop build' then 'makeslop run'\n",
		filepath.Base(pwd))
	fmt.Fprintln(cmd.OutOrStdout(), workspaceDir)
	return nil
}

func newInitCmd(ws *workspace.Workspaces, baseDir string) *cobra.Command {
	var outOfHome bool
	var globalOnly bool

	cmd := &cobra.Command{
		Use:          "init",
		Short:        "Register the current directory as a makeslop workspace",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			return runInit(cmd, ws, baseDir, outOfHome, globalOnly, quiet)
		},
	}
	// --out-of-home and --global-only are scoped to init only (not root-persistent).
	cmd.Flags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")
	cmd.Flags().BoolVar(&globalOnly, "global-only", false,
		"scaffold .makeslop.yaml with project cache overlays disabled (only global ~/.makeslop mounts)")
	return cmd
}

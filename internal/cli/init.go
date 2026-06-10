package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// stampMigratedVersion stamps MigratedVersion = MigrationVersion on a fresh seed.
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

func runInit(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, globalOnly, quiet bool) error {
	chrome := &quietWriter{w: cmd.ErrOrStderr(), quiet: quiet}

	pwd, err := resolvePwd()
	if err != nil {
		return err
	}
	if err := ensureWithinHome(cmd.ErrOrStderr(), pwd, outOfHome); err != nil {
		return err
	}

	// Check before Bootstrap: determines stamp vs nudge path.
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

	initSettings, loadErr := config.Load(baseDir)
	if loadErr != nil {
		return loadErr
	}

	workspaceRoot, _, err := ws.Lookup(initSettings, pwd)
	if err != nil {
		return err
	}
	if err := projectconfig.Scaffold(workspaceRoot, projectconfig.Cache{Content: !globalOnly, Agent: !globalOnly}); err != nil {
		return err
	}

	// Fresh seed: stamp so the new dir is never reported stale.
	// Existing: nudge only — stamping would skip the actual migration.
	if freshSeed {
		if lockErr := stampMigratedVersion(baseDir); lockErr != nil {
			return lockErr
		}
	} else {
		current, latest, stale := config.MigrationStatus(initSettings)
		if stale {
			fmt.Fprintf(chrome,
				"note: your base config is v%d, latest is v%d — run 'makeslop migrate'\n",
				current, latest)
		}
	}

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
	cmd.Flags().BoolVar(&outOfHome, "out-of-home", false,
		"allow running outside the user's home directory")
	cmd.Flags().BoolVar(&globalOnly, "global-only", false,
		"scaffold .makeslop.yaml with project cache overlays disabled (only global ~/.makeslop mounts)")
	return cmd
}

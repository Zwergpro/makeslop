package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zwergpro/makeslop/internal/cache"
	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func resolveProjectCache(baseDir string, layout docker.Layout, cfg projectconfig.Config) (cache.Plan, error) {
	paths, err := projectCachePaths(baseDir, layout.Main.Name, cfg.Cache.Static)
	if err != nil {
		return cache.Plan{}, err
	}

	return cache.Resolve(
		cfg.Cache,
		paths,
		layout,
		cfg.Excludes.DeclaredPaths,
	)
}

func projectCachePaths(baseDir, mainName string, binds []projectconfig.CacheBind) (cache.Sources, error) {
	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return cache.Sources{}, fmt.Errorf("resolve cache base: %w", err)
	}

	// Resolve only the base: resolving managed children would hide their symlinks
	// from the containment checks that must reject them.
	paths := cache.Sources{
		BaseDir: base, WorkspaceDir: filepath.Join(base, config.WorkspacesDir, mainName),
	}
	for _, bind := range binds {
		if bind.Host != "~" && !strings.HasPrefix(bind.Host, "~/") {
			continue
		}

		paths.HostHome, err = os.UserHomeDir()
		if err != nil {
			return cache.Sources{}, fmt.Errorf("resolve cache host home: %w", err)
		}
		break
	}

	return paths, nil
}

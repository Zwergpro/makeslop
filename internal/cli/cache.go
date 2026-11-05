package cli

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

const containerHome = "/home/user"

type cachePaths struct{ BaseDir, WorkspaceDir, HostHome string }

type cachePlan struct {
	Mounts  []docker.Mount
	Dirs    []cacheDir
	entries []string // Parallel to Mounts; identifies the declaration in collision errors.
}

type cacheGroup struct {
	name, host, target string
	entries            []string
}

func resolveCache(
	c projectconfig.CacheMounts,
	paths cachePaths,
	projects []docker.Project,
	declaredExcludes []string,
) (cachePlan, error) {
	// Empty plans retain zero-value slices, like absent project configuration.
	var result cachePlan
	mainRoot := "/workspace/" + projects[0].Name
	if err := result.resolveManaged(c, paths, mainRoot); err != nil {
		return cachePlan{}, err
	}
	if err := result.resolveStatic(c.Static, paths, projects[0].Host); err != nil {
		return cachePlan{}, err
	}

	// Validate the complete plan before the caller is allowed to create sources.
	if err := validateCacheTargets(
		result.Mounts,
		result.entries,
		projects,
		declaredExcludes,
	); err != nil {
		return cachePlan{}, err
	}

	return result, nil
}

func (p *cachePlan) resolveManaged(c projectconfig.CacheMounts, paths cachePaths, mainRoot string) error {
	groups := []cacheGroup{
		{
			name: "shared", host: filepath.Join(paths.BaseDir, "shared"),
			target: containerHome, entries: c.Shared,
		},
		{
			name: "local", host: paths.WorkspaceDir,
			target: mainRoot, entries: c.Local,
		},
	}

	for _, group := range groups {
		if err := p.resolveManagedGroup(group, paths.BaseDir); err != nil {
			return err
		}
	}

	return nil
}

func (p *cachePlan) resolveManagedGroup(group cacheGroup, base string) error {
	for i, name := range group.entries {
		entry := fmt.Sprintf("cache.%s[%d]", group.name, i)
		source := filepath.Join(group.host, filepath.FromSlash(name))
		dir := cacheDir{Base: base, Path: source, Entry: entry}
		if err := checkCacheDir(dir); err != nil {
			return err
		}

		p.Dirs = append(p.Dirs, dir)
		p.entries = append(p.entries, entry)
		p.Mounts = append(p.Mounts, docker.Mount{
			Host: source + "/", Container: path.Join(group.target, name) + "/",
		})
	}

	return nil
}

func (p *cachePlan) resolveStatic(binds []projectconfig.CacheBind, paths cachePaths, projectRoot string) error {
	for i, bind := range binds {
		entry := fmt.Sprintf("cache.static[%d]", i)
		mount, err := resolveStaticCacheMount(
			bind,
			paths,
			projectRoot,
			entry,
		)
		if err != nil {
			return err
		}

		p.entries = append(p.entries, entry)
		p.Mounts = append(p.Mounts, mount)
	}

	return nil
}

func resolveStaticCacheMount(
	bind projectconfig.CacheBind,
	paths cachePaths,
	projectRoot string,
	entry string,
) (docker.Mount, error) {
	host, err := staticCacheHost(bind.Host, paths.HostHome, projectRoot)
	if err != nil {
		return docker.Mount{}, fmt.Errorf("%s: %w", entry, err)
	}
	source, isDir, err := staticCacheSource(host, entry)
	if err != nil {
		return docker.Mount{}, err
	}

	// Container paths use POSIX rules independently of the host filesystem.
	target := bind.Container
	if target == "~" {
		target = containerHome
	}
	if strings.HasPrefix(target, "~/") {
		target = path.Join(containerHome, strings.TrimPrefix(target, "~/"))
	}
	target = path.Clean(target)
	if isDir {
		source += "/"
		target += "/"
	}

	return docker.Mount{Host: source, Container: target}, nil
}

func staticCacheHost(host, home, projectRoot string) (string, error) {
	if host == "~" || strings.HasPrefix(host, "~/") {
		if home == "" {
			return "", fmt.Errorf("host home is unavailable")
		}

		if host == "~" {
			host = home
		} else {
			host = filepath.Join(home, strings.TrimPrefix(host, "~/"))
		}
	}
	if !filepath.IsAbs(host) {
		host = filepath.Join(projectRoot, host)
	}

	return host, nil
}

func staticCacheSource(host, entry string) (string, bool, error) {
	source, err := filepath.EvalSymlinks(host)
	if err != nil {
		return "", false, fmt.Errorf(
			"%s: source %q: %w",
			entry,
			host,
			err,
		)
	}

	info, err := os.Stat(source)
	if err != nil {
		return "", false, fmt.Errorf(
			"%s: source %q: %w",
			entry,
			source,
			err,
		)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s: source %q must be a regular file or directory", entry, source)
	}

	return source, info.IsDir(), nil
}

func resolveProjectCache(baseDir string, projects []docker.Project, cfg projectconfig.Config) (cachePlan, error) {
	paths, err := projectCachePaths(baseDir, projects[0].Name, cfg.Cache.Static)
	if err != nil {
		return cachePlan{}, err
	}

	return resolveCache(
		cfg.Cache,
		paths,
		projects,
		cfg.Excludes.DeclaredPaths,
	)
}

func projectCachePaths(baseDir, mainName string, binds []projectconfig.CacheBind) (cachePaths, error) {
	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return cachePaths{}, fmt.Errorf("resolve cache base: %w", err)
	}

	// Resolve only the base: resolving managed children would hide their symlinks
	// from the containment checks that must reject them.
	paths := cachePaths{
		BaseDir: base, WorkspaceDir: filepath.Join(base, config.WorkspacesDir, mainName),
	}
	for _, bind := range binds {
		if bind.Host != "~" && !strings.HasPrefix(bind.Host, "~/") {
			continue
		}

		paths.HostHome, err = os.UserHomeDir()
		if err != nil {
			return cachePaths{}, fmt.Errorf("resolve cache host home: %w", err)
		}
		break
	}

	return paths, nil
}

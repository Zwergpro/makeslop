package cache

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

const containerHome = "/home/user"

type Sources struct{ BaseDir, WorkspaceDir, HostHome string }

type Plan struct {
	mounts  []docker.Mount
	dirs    []cacheDir
	entries []string // Parallel to Mounts; identifies the declaration in collision errors.
}

type cacheGroup struct {
	name, host, target string
	entries            []projectconfig.CacheDirectory
}

// Resolve validates cache declarations and sources without creating directories.
func Resolve(
	c projectconfig.CacheMounts,
	paths Sources,
	layout docker.Layout,
	declaredExcludes []string,
) (Plan, error) {
	// Empty plans retain zero-value slices, like absent project configuration.
	var result Plan
	mainRoot := layout.Main.ContainerRoot
	if err := result.resolveManaged(c, paths, mainRoot); err != nil {
		return Plan{}, err
	}
	if err := result.resolveStatic(c.Static, paths, layout.Main.ProjectRoot); err != nil {
		return Plan{}, err
	}

	// Validate the complete plan before the caller is allowed to create sources.
	if err := validateCacheTargets(
		result.mounts,
		result.entries,
		layout,
		declaredExcludes,
	); err != nil {
		return Plan{}, err
	}

	return result, nil
}

func (p *Plan) resolveManaged(c projectconfig.CacheMounts, paths Sources, mainRoot string) error {
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

func (p *Plan) resolveManagedGroup(group cacheGroup, base string) error {
	for i, dirConfig := range group.entries {
		name := dirConfig.Name
		entry := fmt.Sprintf("cache.%s[%d]", group.name, i)
		source := filepath.Join(group.host, filepath.FromSlash(name))
		dir := cacheDir{Base: base, Path: source, Entry: entry}
		if err := checkCacheDir(dir); err != nil {
			return err
		}

		p.dirs = append(p.dirs, dir)
		p.entries = append(p.entries, entry)
		p.mounts = append(p.mounts, docker.Mount{
			Type: docker.MountBind,
			Host: source + "/", Container: path.Join(group.target, name) + "/",
			ReadOnly: dirConfig.ReadOnly,
		})
	}

	return nil
}

func (p *Plan) resolveStatic(binds []projectconfig.CacheBind, paths Sources, project docker.ProjectRoot) error {
	for i, bind := range binds {
		entry := fmt.Sprintf("cache.static[%d]", i)
		mount, err := resolveStaticCacheMount(
			bind,
			paths,
			project,
			entry,
		)
		if err != nil {
			return err
		}

		p.entries = append(p.entries, entry)
		p.mounts = append(p.mounts, mount)
	}

	return nil
}

func resolveStaticCacheMount(
	bind projectconfig.CacheBind,
	paths Sources,
	project docker.ProjectRoot,
	entry string,
) (docker.Mount, error) {
	host, err := staticCacheHost(bind.Host, paths.HostHome, project.HostRoot)
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
	if !path.IsAbs(target) {
		target = path.Join(project.ContainerRoot, target)
	}
	target = path.Clean(target)
	if isDir {
		source += "/"
		target += "/"
	}

	return docker.Mount{Type: docker.MountBind, Host: source, Container: target, ReadOnly: bind.ReadOnly}, nil
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

// Mounts returns a copy of the validated mounts in declaration order.
func (p Plan) Mounts() []docker.Mount {
	return slices.Clone(p.mounts)
}

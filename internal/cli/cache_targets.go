package cli

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

type cacheTargetRules struct {
	projects  []docker.Project
	protected []string
}

func validateCacheTargets(
	mounts []docker.Mount,
	entries []string,
	projects []docker.Project,
	excludes []string,
) error {
	rules := cacheTargetRules{projects: projects, protected: cacheProtectedPaths(projects, excludes)}
	for i, mount := range mounts {
		target := path.Clean(mount.Container)
		if err := rules.checkProjectRoots(target, entries[i]); err != nil {
			return err
		}
		if err := rules.checkProtectedPaths(target, entries[i]); err != nil {
			return err
		}

		for j, previous := range mounts[:i] {
			if containerOverlap(target, path.Clean(previous.Container)) {
				return fmt.Errorf(
					"%s: destination %q overlaps %s (%q)",
					entries[i],
					target,
					entries[j],
					previous.Container,
				)
			}
		}
	}

	return nil
}

func cacheProtectedPaths(projects []docker.Project, excludes []string) []string {
	protected := []string{"/tmp", "/proc", "/sys", "/dev"}
	for _, project := range projects {
		root := "/workspace/" + project.Name
		protected = append(protected, root+"/"+projectconfig.Filename, root+"/.git/hooks")
	}

	mainRoot := "/workspace/" + projects[0].Name
	for _, exclude := range excludes {
		protected = append(protected, path.Join(mainRoot, filepath.ToSlash(exclude)))
	}

	return protected
}

func (r cacheTargetRules) checkProjectRoots(target, entry string) error {
	// Root checks are directional: children of the main project/home are allowed,
	// but mounting an ancestor would hide the root itself.
	for _, root := range []string{containerHome, "/workspace"} {
		if withinContainer(root, target) {
			return fmt.Errorf(
				"%s: destination %q replaces protected root %q",
				entry,
				target,
				root,
			)
		}
	}

	for i, project := range r.projects {
		root := "/workspace/" + project.Name
		replacesRoot := withinContainer(root, target)
		insideJoin := i > 0 && withinContainer(target, root)
		if replacesRoot || insideJoin {
			return fmt.Errorf(
				"%s: destination %q conflicts with project %q",
				entry,
				target,
				root,
			)
		}
	}

	return nil
}

func (r cacheTargetRules) checkProtectedPaths(target, entry string) error {
	// Unlike root checks, masks and policy paths prohibit overlap both ways.
	for _, reserved := range r.protected {
		if containerOverlap(target, reserved) {
			return fmt.Errorf(
				"%s: destination %q overlaps protected or excluded path %q",
				entry,
				target,
				reserved,
			)
		}
	}

	return nil
}

func withinContainer(p, root string) bool {
	// Compare complete path segments: /opt/toolbox is not inside /opt/tool.
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

func containerOverlap(a, b string) bool {
	return withinContainer(a, b) || withinContainer(b, a)
}

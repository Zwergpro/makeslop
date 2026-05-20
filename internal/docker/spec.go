// Package docker assembles and executes the `docker run` invocation. Argv
// assembly (BuildSpec, Spec.Args) is pure; exec lives in run.go.
package docker

import (
	"path/filepath"
	"strings"

	"github.com/Zwergpro/makeslop/internal/config"
)

// Options is the caller-supplied input to BuildSpec. All path fields are
// expected to be absolute and EvalSymlinks-evaluated by the caller — the cobra
// layer already enforces this for ProjectRoot/BaseDir, matching the same
// precondition workspace.Workspaces.Lookup documents.
type Options struct {
	ProjectRoot   string // host path mounted at /workspace/<WorkspaceName>
	WorkspaceName string // e.g. "makeslop-ab12cd"; derived from cache dir basename
	BaseDir       string // ~/.makeslop
	Image         string
	Command       string // shell to exec inside the container
}

// Mount is a single host:container bind mount. Trailing slashes are preserved verbatim.
type Mount struct {
	Host, Container string
}

// Spec is the deterministic shape of a `docker run` invocation; Args() is a pure projection.
type Spec struct {
	Image   string
	Command string
	Workdir string
	Mounts  []Mount
	Tmpfs   []string
	CapDrop []string
	SecOpt  []string
}

// BuildSpec is pure: same Options → same Spec. The mount list is emitted in a
// fixed explicit order (project root, then global agent paths under BaseDir,
// then per-workspace agent paths under BaseDir/workspaces/<name>) so callers
// and tests can rely on argv ordering.
func BuildSpec(o Options) Spec {
	workspacePath := "/workspace/" + o.WorkspaceName
	workspaceHost := filepath.Join(o.BaseDir, config.WorkspacesDir, o.WorkspaceName)

	// Trailing slashes on directory mounts are intentional — they match the
	// reference claude.sh exactly, and a trailing slash on the host side coaxes
	// docker into failing fast if the path is unexpectedly a file.
	mounts := []Mount{
		{Host: o.ProjectRoot, Container: workspacePath},
		{Host: filepath.Join(o.BaseDir, ".claude") + "/", Container: "/home/user/.claude/"},
		{Host: filepath.Join(o.BaseDir, ".claude.json"), Container: "/home/user/.claude.json"},
		{Host: filepath.Join(o.BaseDir, ".codex") + "/", Container: "/home/user/.codex/"},
		{Host: filepath.Join(workspaceHost, ".claude") + "/", Container: workspacePath + "/.claude/"},
		{Host: filepath.Join(workspaceHost, ".codex") + "/", Container: workspacePath + "/.codex/"},
		{Host: filepath.Join(workspaceHost, "docs") + "/", Container: workspacePath + "/docs/"},
		{Host: filepath.Join(workspaceHost, "CLAUDE.md"), Container: workspacePath + "/CLAUDE.md"},
	}

	return Spec{
		Image:   o.Image,
		Command: o.Command,
		Workdir: workspacePath,
		Mounts:  mounts,
		Tmpfs:   []string{"/tmp:size=100m"},
		CapDrop: []string{"ALL"},
		SecOpt:  []string{"no-new-privileges"},
	}
}

// Args returns argv starting with "run". Mount source/target fields use RFC 4180
// CSV quoting so paths containing ',' or '"' parse unambiguously.
func (s Spec) Args() []string {
	var args []string
	args = append(args, "run", "--rm", "-it")
	args = append(args, "--workdir", s.Workdir)
	for _, t := range s.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	for _, c := range s.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	for _, so := range s.SecOpt {
		args = append(args, "--security-opt", so)
	}
	for _, m := range s.Mounts {
		args = append(args, "--mount",
			"type=bind,"+csvField("source="+m.Host)+","+csvField("target="+m.Container))
	}
	args = append(args, s.Image, s.Command)
	return args
}

// csvField returns s as a single RFC 4180 CSV field: unquoted when it contains
// no CSV-special characters, otherwise wrapped in `"` with embedded `"` doubled.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

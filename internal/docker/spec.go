package docker

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// Options is the caller-supplied input to BuildSpec. Path fields must be
// absolute and EvalSymlinks-evaluated.
type Options struct {
	ProjectRoot   string // host path mounted at /workspace/<WorkspaceName>
	WorkspaceName string // e.g. "makeslop-ab12cd"
	BaseDir       string // ~/.makeslop
	// WorkspaceHost is the per-workspace cache directory on the host
	// (e.g. ~/.makeslop/workspaces/<WorkspaceName>). Caller computes this;
	// BuildSpec uses it directly for cache overlay mounts.
	WorkspaceHost string
	Image         string
	Command       string // shell to exec inside the container
	// MaskedFiles: absolute host paths under ProjectRoot to shadow with /dev/null.
	MaskedFiles []string
	// MaskedDirs: absolute host paths under ProjectRoot to replace with tmpfs.
	MaskedDirs []string

	// TmpDirSize is passed verbatim to --tmpfs /tmp:size=<TmpDirSize>; config.Load
	// owns the default, BuildSpec does not re-default.
	TmpDirSize string

	// Env holds "KEY=VALUE" pairs to inject; copied verbatim (caller sorts).
	Env []string

	// MountAgentCache gates the per-workspace agent-state cache overlays
	// (workspaceHost/.claude/, .codex/). The global ~/.makeslop equivalents are
	// always present regardless.
	MountAgentCache bool

	// MountContentCache gates the per-workspace content cache overlays
	// (workspaceHost/docs/, CLAUDE.md); false lets the project's own files show.
	MountContentCache bool

	// ProtectProjectConfig mounts <ProjectRoot>/.makeslop.yaml read-only over
	// itself inside the container, preventing the agent from rewriting the sandbox
	// policy. Only set when the file actually exists on the host (a missing bind
	// source would fail container create).
	ProtectProjectConfig bool

	// MaskGitHooks overlays <workspacePath>/.git/hooks with a tmpfs, preventing
	// the agent from planting hooks that execute on the host. Only set when
	// <ProjectRoot>/.git is a directory (worktrees/submodule gitfiles are skipped
	// — their hooks directory lives outside the workspace).
	MaskGitHooks bool
}

// filterOut returns s without the first occurrence of exclude; the input is
// returned unmodified when exclude is absent.
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

// Mount is a single docker mount entry. Type "" or "bind" → bind; "tmpfs" →
// tmpfs (Host ignored); "volume" → volume (Host is the volume name).
type Mount struct {
	Type            string
	Host, Container string
	ReadOnly        bool
}

// Spec is the deterministic shape of a `docker run` invocation.
type Spec struct {
	Image   string
	Command string
	Workdir string
	Env     []string // "KEY=VALUE" pairs; nil/empty → no -e flags
	Mounts  []Mount
	Tmpfs   []string
	CapDrop []string
	SecOpt  []string
}

// BuildSpec is pure: same Options → same Spec. Mask overlays must follow the
// directory bind they shadow so docker's argv-order evaluation makes them win;
// disabled groups are omitted, never reordered.
func BuildSpec(o Options) Spec {
	workspacePath := "/workspace/" + o.WorkspaceName

	// Trailing slashes on directory mounts are intentional — they match the
	// reference claude.sh, and coax docker into failing fast if the host path
	// is unexpectedly a file.
	mounts := []Mount{
		{Host: o.ProjectRoot, Container: workspacePath},
		{Host: filepath.Join(o.BaseDir, ".claude") + "/", Container: "/home/user/.claude/"},
		{Host: filepath.Join(o.BaseDir, ".claude.json"), Container: "/home/user/.claude.json"},
		{Host: filepath.Join(o.BaseDir, ".codex") + "/", Container: "/home/user/.codex/"},
	}

	// Sandbox-policy mounts: inserted at a fixed point after the 4 base mounts
	// and before any cache overlays. This ensures the overlay wins over the rw
	// project bind regardless of cache flag state.
	maskedFiles := o.MaskedFiles
	if o.ProtectProjectConfig {
		configHost := filepath.Join(o.ProjectRoot, ".makeslop.yaml")
		mounts = append(mounts, Mount{
			Host:      configHost,
			Container: workspacePath + "/.makeslop.yaml",
			ReadOnly:  true,
		})
		// Docker applies mounts last-write-wins: a /dev/null mask emitted below
		// for the config file (e.g. a broad scan pattern like "*.yaml") would
		// silently override the read-only self-bind, so drop it here.
		maskedFiles = filterOut(maskedFiles, configHost)
	}
	if o.MaskGitHooks {
		mounts = append(mounts, Mount{
			Type:      "tmpfs",
			Container: workspacePath + "/.git/hooks",
		})
	}

	if o.MountAgentCache {
		mounts = append(mounts,
			Mount{Host: filepath.Join(o.WorkspaceHost, ".claude") + "/", Container: workspacePath + "/.claude/"},
			Mount{Host: filepath.Join(o.WorkspaceHost, ".codex") + "/", Container: workspacePath + "/.codex/"},
		)
	}

	if o.MountContentCache {
		mounts = append(mounts,
			Mount{Host: filepath.Join(o.WorkspaceHost, "docs") + "/", Container: workspacePath + "/docs/"},
			Mount{Host: filepath.Join(o.WorkspaceHost, "CLAUDE.md"), Container: workspacePath + "/CLAUDE.md"},
		)
	}

	for _, host := range maskedFiles {
		// Caller guarantees host is under ProjectRoot; Rel never errors on POSIX.
		rel, _ := filepath.Rel(o.ProjectRoot, host)
		mounts = append(mounts, Mount{
			Host:      "/dev/null",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}

	for _, host := range o.MaskedDirs {
		// Caller guarantees host is under ProjectRoot; Rel never errors on POSIX.
		rel, _ := filepath.Rel(o.ProjectRoot, host)
		mounts = append(mounts, Mount{
			Type:      "tmpfs",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}

	return Spec{
		Image:   o.Image,
		Command: o.Command,
		Workdir: workspacePath,
		Env:     o.Env,
		Mounts:  mounts,
		Tmpfs:   []string{"/tmp:size=" + o.TmpDirSize},
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
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	for _, m := range s.Mounts {
		switch m.Type {
		case "tmpfs":
			args = append(args, "--mount",
				"type=tmpfs,"+csvField("target="+m.Container))
		case "volume":
			val := "type=volume," + csvField("source="+m.Host) + "," + csvField("target="+m.Container)
			if m.ReadOnly {
				val += ",readonly"
			}
			args = append(args, "--mount", val)
		default: // "" or "bind"
			val := "type=bind," + csvField("source="+m.Host) + "," + csvField("target="+m.Container)
			if m.ReadOnly {
				val += ",readonly"
			}
			args = append(args, "--mount", val)
		}
	}
	args = append(args, s.Image, s.Command)
	return args
}

var shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_./:=,@+-]+$`)

// shellQuote returns s safe for POSIX shell inclusion: bare when safe, else
// single-quoted (embedded quotes escaped the usual POSIX way).
func shellQuote(s string) string {
	if s != "" && shellSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellCommand renders s as a multi-line backslash-continued `docker run` command.
func (s Spec) ShellCommand() string {
	args := s.Args() // starts with "run", not "docker"

	var lines []string
	lines = append(lines, "docker run")

	i := 1 // skip "run" — already in "docker run" prefix
	for i < len(args)-2 {
		tok := args[i]
		switch tok {
		case "--workdir", "--tmpfs", "--cap-drop", "--security-opt", "--mount", "-e":
			lines = append(lines, "  "+shellQuote(tok)+" "+shellQuote(args[i+1]))
			i += 2
		default:
			lines = append(lines, "  "+shellQuote(tok))
			i++
		}
	}
	// Explicit tail lines: a flag-shaped image name must not be parsed as a flag.
	lines = append(lines, "  "+shellQuote(args[len(args)-2]))
	lines = append(lines, "  "+shellQuote(args[len(args)-1]))

	var sb strings.Builder
	for j, line := range lines {
		sb.WriteString(line)
		if j < len(lines)-1 {
			sb.WriteString(" \\")
		}
		sb.WriteByte('\n')
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// ContainerConfig returns the SDK container.Config for this Spec.
func (s Spec) ContainerConfig() *container.Config {
	return &container.Config{
		Image:        s.Image,
		Cmd:          []string{s.Command},
		WorkingDir:   s.Workdir,
		Env:          s.Env,
		Tty:          true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}
}

// HostConfig returns the SDK container.HostConfig for this Spec.
func (s Spec) HostConfig() *container.HostConfig {
	return &container.HostConfig{
		AutoRemove:  true,
		CapDrop:     s.CapDrop,
		SecurityOpt: s.SecOpt,
		Tmpfs:       tmpfsMap(s.Tmpfs),
		Mounts:      mountsFor(s.Mounts),
	}
}

// tmpfsMap converts "target:opts" (or bare "target") entries into the
// container.HostConfig.Tmpfs map. Splits on the first colon only — matching
// docker, target paths may not contain ':'.
func tmpfsMap(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if idx := strings.Index(e, ":"); idx >= 0 {
			m[e[:idx]] = e[idx+1:]
		} else {
			m[e] = ""
		}
	}
	return m
}

// mountsFor translates []Mount into the SDK []mount.Mount form.
func mountsFor(mounts []Mount) []mount.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]mount.Mount, len(mounts))
	for i, m := range mounts {
		switch m.Type {
		case "tmpfs":
			out[i] = mount.Mount{
				Type:   mount.TypeTmpfs,
				Target: m.Container,
			}
		case "volume":
			// Host carries the Docker volume name for volume mounts.
			out[i] = mount.Mount{
				Type:     mount.TypeVolume,
				Source:   m.Host,
				Target:   m.Container,
				ReadOnly: m.ReadOnly,
			}
		default: // "" or "bind"
			out[i] = mount.Mount{
				Type:     mount.TypeBind,
				Source:   m.Host,
				Target:   m.Container,
				ReadOnly: m.ReadOnly,
			}
		}
	}
	return out
}

// BuildOptions is the caller-supplied input to Build. Path fields must be absolute.
type BuildOptions struct {
	Image          string   // -t tag (required)
	DockerfilePath string   // -f path (required)
	ContextDir     string   // empty ⇒ Build auto-creates a temp dir
	NoCache        bool     // --no-cache
	BuildArgs      []string // forwarded as build arguments
	Quiet          bool     // suppress build progress output
}

// csvField returns s as a single RFC 4180 CSV field: unquoted when free of
// CSV-special characters, otherwise wrapped in `"` with embedded `"` doubled.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

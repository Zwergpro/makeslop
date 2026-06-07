// Package docker assembles and executes the `docker run` invocation. Argv
// assembly (BuildSpec, Spec.Args, Spec.ShellCommand) is pure; exec lives in
// run.go. Pure SDK-struct projections (Spec.ContainerConfig, Spec.HostConfig)
// are also pure — they never touch the filesystem or exec anything — and
// produce the same result for the same Spec, consistent with the CLAUDE.md
// pure/impure split contract.
//
// BuildSpec emits three mount groups:
//   - global (always): BaseDir/.claude/, BaseDir/.claude.json, BaseDir/.codex/
//   - agent-state cache (when MountAgentCache is true): workspace .claude/ + .codex/
//   - content cache (when MountContentCache is true): workspace docs/ + CLAUDE.md
package docker

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// Options is the caller-supplied input to BuildSpec. Path fields must be
// absolute and EvalSymlinks-evaluated.
type Options struct {
	ProjectRoot   string // host path mounted at /workspace/<WorkspaceName>
	WorkspaceName string // e.g. "makeslop-ab12cd"
	BaseDir       string // ~/.makeslop
	Image         string
	Command       string // shell to exec inside the container
	// MaskedFiles: absolute host paths under ProjectRoot to shadow with /dev/null.
	// Under-root guarantee is the caller's. Nil or empty is a no-op.
	MaskedFiles []string
	// MaskedDirs: absolute host paths under ProjectRoot to replace with tmpfs.
	// Under-root guarantee is the caller's. Nil or empty is a no-op.
	MaskedDirs []string

	// TmpDirSize is the size constraint passed verbatim to --tmpfs /tmp:size=<TmpDirSize>.
	// config.Load is the single source of the default ("100m"); BuildSpec uses it
	// verbatim without re-defaulting, matching how Image/Command are handled.
	TmpDirSize string

	// MountAgentCache controls whether the per-workspace agent-state cache
	// directories are mounted into the container:
	//
	//	workspaceHost/.claude/ → workspacePath/.claude/
	//	workspaceHost/.codex/  → workspacePath/.codex/
	//
	// When false, those two per-workspace overlays are omitted; the global
	// ~/.makeslop/.claude/ and ~/.makeslop/.codex/ mounts are always present.
	// Zero value is false; set from projectconfig.Cache by the caller
	// (absent cache: block ⇒ true).
	MountAgentCache bool

	// MountContentCache controls whether the per-workspace content cache
	// directories are mounted into the container:
	//
	//	workspaceHost/docs/     → workspacePath/docs/
	//	workspaceHost/CLAUDE.md → workspacePath/CLAUDE.md
	//
	// When false, those two per-workspace overlays are omitted, allowing the
	// project's own docs/ and CLAUDE.md to be visible inside the container.
	// Zero value is false; set from projectconfig.Cache by the caller
	// (absent cache: block ⇒ true).
	MountContentCache bool
}

// Mount is a single docker mount entry.
//
// Type: "" or "bind" → type=bind; "tmpfs" → type=tmpfs (Host ignored);
// "volume" → type=volume (Host is the volume name, ReadOnly appends ",readonly").
// ReadOnly: when true, appends ",readonly" to bind and volume mounts; zero value
// is backward-compatible (existing mounts render byte-identically).
type Mount struct {
	Type            string
	Host, Container string
	ReadOnly        bool
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

// BuildSpec is pure: same Options → same Spec. Mount order is deterministic:
//  1. Group 1: project root bind + global agent binds (BaseDir/.claude/, .claude.json, .codex/) — always present
//  2. Group 2: agent-state cache mounts (workspaceHost/.claude/, .codex/) — when MountAgentCache is true
//  3. Group 3: content cache mounts (workspaceHost/docs/, CLAUDE.md) — when MountContentCache is true
//  4. MaskedFiles /dev/null overlays
//  5. MaskedDirs tmpfs overlays
//
// Overlays (4–5) follow the directory bind they shadow so docker's argv-order
// evaluation makes them win. Entries are omitted when their group is disabled —
// they are never reordered.
func BuildSpec(o Options) Spec {
	workspacePath := "/workspace/" + o.WorkspaceName
	workspaceHost := filepath.Join(o.BaseDir, config.WorkspacesDir, o.WorkspaceName)

	// Trailing slashes on directory mounts are intentional — they match the
	// reference claude.sh exactly, and a trailing slash on the host side coaxes
	// docker into failing fast if the path is unexpectedly a file.

	// Group 1: project root + global mounts (always present).
	mounts := []Mount{
		{Host: o.ProjectRoot, Container: workspacePath},
		{Host: filepath.Join(o.BaseDir, ".claude") + "/", Container: "/home/user/.claude/"},
		{Host: filepath.Join(o.BaseDir, ".claude.json"), Container: "/home/user/.claude.json"},
		{Host: filepath.Join(o.BaseDir, ".codex") + "/", Container: "/home/user/.codex/"},
	}

	// Group 2: per-workspace agent-state cache (config-gated).
	if o.MountAgentCache {
		mounts = append(mounts,
			Mount{Host: filepath.Join(workspaceHost, ".claude") + "/", Container: workspacePath + "/.claude/"},
			Mount{Host: filepath.Join(workspaceHost, ".codex") + "/", Container: workspacePath + "/.codex/"},
		)
	}

	// Group 3: per-workspace content cache (config-gated).
	if o.MountContentCache {
		mounts = append(mounts,
			Mount{Host: filepath.Join(workspaceHost, "docs") + "/", Container: workspacePath + "/docs/"},
			Mount{Host: filepath.Join(workspaceHost, "CLAUDE.md"), Container: workspacePath + "/CLAUDE.md"},
		)
	}

	for _, host := range o.MaskedFiles {
		// security.Scan guarantees host is under ProjectRoot; Rel never errors on POSIX.
		rel, _ := filepath.Rel(o.ProjectRoot, host)
		mounts = append(mounts, Mount{
			Host:      "/dev/null",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}

	for _, host := range o.MaskedDirs {
		// projectconfig.Load guarantees host is under ProjectRoot; Rel never errors on POSIX.
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
		default:
			// "" or "bind" both render as type=bind.
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

// shellQuote returns s safe for POSIX shell inclusion: bare if alphanumeric-safe,
// empty string as ”, otherwise single-quoted with '\” escaping.
func shellQuote(s string) string {
	if s != "" && shellSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellCommand renders s as a multi-line backslash-continued `docker run`
// command. Pure: same Spec → same string.
func (s Spec) ShellCommand() string {
	args := s.Args() // starts with "run", not "docker"

	var lines []string
	lines = append(lines, "docker run")

	i := 1 // skip "run" — already in "docker run" prefix
	for i < len(args)-2 {
		tok := args[i]
		switch tok {
		case "--network", "--workdir", "--tmpfs", "--cap-drop", "--security-opt", "--mount", "-e":
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
// Pure: same Spec → same *container.Config. Never touches the filesystem.
func (s Spec) ContainerConfig() *container.Config {
	return &container.Config{
		Image:        s.Image,
		Cmd:          []string{s.Command},
		WorkingDir:   s.Workdir,
		Tty:          true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}
}

// HostConfig returns the SDK container.HostConfig for this Spec.
// Pure: same Spec → same *container.HostConfig. Never touches the filesystem.
func (s Spec) HostConfig() *container.HostConfig {
	return &container.HostConfig{
		AutoRemove:  true,
		CapDrop:     s.CapDrop,
		SecurityOpt: s.SecOpt,
		Tmpfs:       tmpfsMap(s.Tmpfs),
		Mounts:      mountsFor(s.Mounts),
	}
}

// tmpfsMap converts a slice of "target:opts" (or "target" with no colon)
// entries into the map[string]string that container.HostConfig.Tmpfs expects.
// The split is on the first colon only, so target paths containing ':'
// are not supported (docker itself doesn't support them either).
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

// mountsFor translates the package-local []Mount slice into the SDK
// []mount.Mount form used by container.HostConfig.
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
			// Host field carries the Docker volume name for volume mounts.
			out[i] = mount.Mount{
				Type:     mount.TypeVolume,
				Source:   m.Host,
				Target:   m.Container,
				ReadOnly: m.ReadOnly,
			}
		default:
			// "" or "bind" both map to bind mount.
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

// BuildOptions is the caller-supplied input to Build. All path fields must be
// absolute. ContextDir may be left empty; Build will create a temporary empty
// directory automatically.
type BuildOptions struct {
	Image          string   // -t tag (required)
	DockerfilePath string   // -f path (required)
	ContextDir     string   // positional build context; empty means Build auto-creates a temp dir
	NoCache        bool     // --no-cache when true
	BuildArgs      []string // each forwarded as a build argument to the daemon
	Quiet          bool     // --quiet: suppress build progress output
}

// csvField returns s as a single RFC 4180 CSV field: unquoted when it contains
// no CSV-special characters, otherwise wrapped in `"` with embedded `"` doubled.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

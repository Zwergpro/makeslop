// Package docker assembles and executes the `docker run` invocation. Argv
// assembly (BuildSpec, Spec.Args) is pure; exec lives in run.go.
package docker

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Zwergpro/makeslop/internal/config"
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

	// ProxySocketHost is the host-side unix socket for the forward proxy. When
	// non-empty, BuildSpec emits --network none, a read-only socket bind mount,
	// and HTTP_PROXY/HTTPS_PROXY pointing at ProxySocketContainer. Empty means
	// default bridge networking.
	ProxySocketHost string
	// ProxySocketContainer is the in-container socket path; use /tmp (guaranteed
	// writable via --tmpfs /tmp). The env vars reference it via unix:// URL.
	ProxySocketContainer string
}

// Mount is a single docker mount entry.
//
// Type: "" or "bind" → type=bind; "tmpfs" → type=tmpfs (Host ignored).
// ReadOnly: when true, appends ",readonly" to bind mounts; zero value is
// backward-compatible (existing mounts render byte-identically).
type Mount struct {
	Type            string
	Host, Container string
	ReadOnly        bool
}

// Spec is the deterministic shape of a `docker run` invocation; Args() is a pure projection.
type Spec struct {
	Image       string
	Command     string
	Workdir     string
	Mounts      []Mount
	Tmpfs       []string
	CapDrop     []string
	SecOpt      []string
	NetworkMode string   // e.g. "none"; empty ⇒ default docker networking
	Env         []string // KEY=VALUE pairs emitted as -e flags
}

// BuildSpec is pure: same Options → same Spec. Mount order is deterministic:
//  1. project root bind
//  2. agent path binds
//  3. MaskedFiles /dev/null overlays
//  4. MaskedDirs tmpfs overlays
//  5. proxy socket bind (when ProxySocketHost is set)
//
// Overlays (3–4) follow the directory bind they shadow so docker's argv-order
// evaluation makes them win.
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

	spec := Spec{
		Image:   o.Image,
		Command: o.Command,
		Workdir: workspacePath,
		Mounts:  mounts,
		Tmpfs:   []string{"/tmp:size=100m"},
		CapDrop: []string{"ALL"},
		SecOpt:  []string{"no-new-privileges"},
	}

	if o.ProxySocketHost != "" {
		spec.NetworkMode = "none"
		unixURL := "unix://" + o.ProxySocketContainer
		spec.Env = []string{
			"HTTP_PROXY=" + unixURL,
			"HTTPS_PROXY=" + unixURL,
		}
		spec.Mounts = append(spec.Mounts, Mount{
			Host:      o.ProxySocketHost,
			Container: o.ProxySocketContainer,
			ReadOnly:  true,
		})
	}

	return spec
}

// Args returns argv starting with "run". Mount source/target fields use RFC 4180
// CSV quoting so paths containing ',' or '"' parse unambiguously.
func (s Spec) Args() []string {
	var args []string
	args = append(args, "run", "--rm", "-it")
	if s.NetworkMode != "" {
		args = append(args, "--network", s.NetworkMode)
	}
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
		if m.Type == "tmpfs" {
			args = append(args, "--mount",
				"type=tmpfs,"+csvField("target="+m.Container))
		} else {
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

// BuildOptions is the caller-supplied input to BuildArgv. All path fields must
// be absolute. ContextDir is required and non-empty (see package-level note).
type BuildOptions struct {
	Image          string   // -t tag (required)
	DockerfilePath string   // -f path (required)
	ContextDir     string   // positional build context (required, non-empty)
	NoCache        bool     // --no-cache when true
	BuildArgs      []string // each forwarded as: --build-arg <entry>
}

// BuildArgv returns argv starting with "build" for the given BuildOptions.
// Argv order is deterministic:
//
//	build [--no-cache] -f <dockerfile> -t <image> (--build-arg <entry>)* <contextDir>
//
// ContextDir must be non-empty; BuildArgv is a pure projection and never
// invents a path. The caller (Build) is responsible for providing it.
func BuildArgv(o BuildOptions) []string {
	var args []string
	args = append(args, "build")
	if o.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, "-f", o.DockerfilePath)
	args = append(args, "-t", o.Image)
	for _, entry := range o.BuildArgs {
		args = append(args, "--build-arg", entry)
	}
	args = append(args, o.ContextDir)
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

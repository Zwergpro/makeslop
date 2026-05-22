// Package docker assembles and executes the `docker run` invocation. Argv
// assembly (BuildSpec, Spec.Args) is pure; exec lives in run.go.
package docker

import (
	"path/filepath"
	"regexp"
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
	// MaskedFiles is the list of absolute host paths under ProjectRoot whose
	// container counterparts should be shadowed by /dev/null. The under-root
	// guarantee is the caller's (security.Scan enforces it). Caller-provided
	// order is preserved in the emitted argv. Nil or empty is a no-op.
	MaskedFiles []string
	// MaskedDirs is the list of absolute host paths under ProjectRoot whose
	// container counterparts should be replaced by an empty in-memory tmpfs
	// mount. The under-root guarantee is the caller's (projectconfig.Load
	// enforces it). Caller-provided order is preserved in the emitted argv.
	// Nil or empty is a no-op.
	MaskedDirs []string
}

// Mount is a single docker mount entry. Trailing slashes are preserved verbatim.
//
// Type selects the mount type: "" (zero value) and "bind" both render as
// type=bind with source= and target=. "tmpfs" renders as type=tmpfs,target=
// only — Host is ignored for tmpfs mounts. Any future unrecognized value falls
// through to bind (Mount is constructed only inside this package, so callers
// cannot inject an unknown type in practice).
type Mount struct {
	// Type is the docker mount type. "" (zero value) means "bind".
	// "tmpfs" is the only other recognized value; Host is unused for tmpfs.
	Type            string
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
// fixed explicit order so callers and tests can rely on argv ordering:
//  1. project root bind mount
//  2. global and per-workspace agent path bind mounts
//  3. /dev/null overlay bind mounts (one per MaskedFiles entry)
//  4. tmpfs overlay mounts (one per MaskedDirs entry)
//
// Both overlay groups come after the directory bind they shadow, so docker's
// argv-order evaluation makes the overlays win.
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
		// Precondition: host is absolute and under ProjectRoot (security.Scan
		// enforces this). filepath.Rel cannot error on two clean absolute POSIX
		// paths on the same volume.
		rel, _ := filepath.Rel(o.ProjectRoot, host)
		mounts = append(mounts, Mount{
			Host:      "/dev/null",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}

	for _, host := range o.MaskedDirs {
		// Precondition: host is absolute and under ProjectRoot (projectconfig.Load
		// enforces this). filepath.Rel cannot error on two clean absolute POSIX
		// paths on the same volume.
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
		if m.Type == "tmpfs" {
			args = append(args, "--mount",
				"type=tmpfs,"+csvField("target="+m.Container))
		} else {
			args = append(args, "--mount",
				"type=bind,"+csvField("source="+m.Host)+","+csvField("target="+m.Container))
		}
	}
	args = append(args, s.Image, s.Command)
	return args
}

// shellSafeRe matches tokens that need no shell quoting: non-empty strings
// composed only of alphanumerics and the safe punctuation set.
var shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_./:=,@+-]+$`)

// shellQuote returns s in a form safe for inclusion in a POSIX shell command
// line. Tokens matching shellSafeRe are returned bare. An empty string returns
// ''. All other strings are wrapped in single quotes with embedded single
// quotes rewritten as the POSIX idiom '\''.
func shellQuote(s string) string {
	if s != "" && shellSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellCommand returns a multi-line, backslash-continued shell command
// equivalent to `docker` invoked with s.Args(). The first line is `docker run
// \` and each subsequent token group is two-space-indented. Flag/value pairs
// share one line; single-token flags are on their own line. The image and
// command each occupy their own trailing line; the image line carries a
// trailing ` \` and the command line does not. Tokens that contain
// shell-special characters are single-quoted with POSIX-safe escaping (see
// shellQuote). Pure: same Spec → same string.
func (s Spec) ShellCommand() string {
	// Build the same logical groups as Args() but in line-oriented form.
	// We collect lines and join them with " \\\n" between consecutive lines,
	// with the final line having no trailing continuation.
	args := s.Args() // starts with "run", not "docker"

	var lines []string
	lines = append(lines, "docker run")

	// i := 1 skips the leading "run" token — we already prepended "docker run".
	// The last two args are always Image and Command; handle them explicitly
	// below so a paired-flag name in Image cannot be misread as a flag.
	i := 1 // skip "run"
	for i < len(args)-2 {
		tok := args[i]
		switch tok {
		case "--workdir", "--tmpfs", "--cap-drop", "--security-opt", "--mount":
			lines = append(lines, "  "+shellQuote(tok)+" "+shellQuote(args[i+1]))
			i += 2
		default:
			lines = append(lines, "  "+shellQuote(tok))
			i++
		}
	}
	// Emit image and command as explicit trailing lines regardless of their value.
	lines = append(lines, "  "+shellQuote(args[len(args)-2]))
	lines = append(lines, "  "+shellQuote(args[len(args)-1]))

	// Join with " \" continuation on all but the last line.
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

// csvField returns s as a single RFC 4180 CSV field: unquoted when it contains
// no CSV-special characters, otherwise wrapped in `"` with embedded `"` doubled.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

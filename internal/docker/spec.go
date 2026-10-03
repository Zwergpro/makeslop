package docker

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// Project.Host must be absolute and symlink-resolved before BuildSpec.
type Project struct {
	Host     string
	Name     string
	Label    string
	ReadOnly bool // ignored for Projects[0]

	MaskedFiles []string
	MaskedDirs  []string

	// Set only for an existing file; a missing bind source fails container creation.
	ProtectConfig bool

	// Only directory-backed .git can have hooks masked within this mount.
	MaskGitHooks bool
}

// Options is the caller-supplied input to BuildSpec.
type Options struct {
	// The caller guarantees a main project at index 0, unique names and disjoint roots.
	Projects []Project
	BaseDir  string // ~/.makeslop
	// WorkspaceHost is the per-workspace cache directory on the host
	// (e.g. ~/.makeslop/workspaces/<Projects[0].Name>). Caller computes this;
	// BuildSpec uses it directly for cache overlay mounts.
	WorkspaceHost string
	Image         string
	Command       string // shell to exec inside the container

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

	// NetworkMode is passed verbatim as --network / HostConfig.NetworkMode
	// (bridge, host, none, container:<x>, or a network name). Empty means the
	// Docker default. Mutually exclusive with Networks (projectconfig enforces).
	NetworkMode string

	// Networks lists networks to attach at create time, in order; the first is
	// the primary (HostConfig.NetworkMode). Copied verbatim.
	Networks []string
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

	NetworkMode string   // "" → no --network flag, Docker default bridge
	Networks    []string // one --network per entry; first is primary

	// Presentation only; argv and SDK projections ignore these labels.
	Sections []Section
}

type Section struct {
	Label string
	Start int // index into Spec.Mounts
}

// BuildSpec is pure: same Options → same Spec. Mask overlays must follow the
// directory bind they shadow so docker's argv-order evaluation makes them win;
// disabled groups are omitted, never reordered.
func BuildSpec(o Options) Spec {
	main := o.Projects[0]
	main.ReadOnly = false
	workspacePath := main.containerPath()

	// Directory trailing slashes make Docker reject unexpected files early.
	mounts := []Mount{projectBind(main)}
	mounts = append(mounts,
		Mount{Host: filepath.Join(o.BaseDir, ".claude") + "/", Container: "/home/user/.claude/"},
		Mount{Host: filepath.Join(o.BaseDir, ".claude.json"), Container: "/home/user/.claude.json"},
		Mount{Host: filepath.Join(o.BaseDir, ".codex") + "/", Container: "/home/user/.codex/"},
	)

	// The policy mounts must shadow the writable project bind.
	mounts = append(mounts, projectSandbox(main)...)

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

	mounts = append(mounts, projectMasks(main, main.ProtectConfig)...)

	// An ro join needs no policy self-bind, so its config mask remains effective.
	var sections []Section
	if len(o.Projects) > 1 {
		sections = append(sections, Section{Label: main.Label, Start: 0})
	}
	for _, j := range o.Projects[1:] {
		sections = append(sections, Section{Label: j.Label, Start: len(mounts)})
		mounts = append(mounts, projectBind(j))
		if !j.ReadOnly {
			mounts = append(mounts, projectSandbox(j)...)
		}
		mounts = append(mounts, projectMasks(j, !j.ReadOnly && j.ProtectConfig)...)
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

		NetworkMode: o.NetworkMode,
		Networks:    o.Networks,

		Sections: sections,
	}
}

func projectBind(p Project) Mount {
	return Mount{Host: p.Host, Container: p.containerPath(), ReadOnly: p.ReadOnly}
}

// projectConfigFile mirrors projectconfig.Filename; docker does not import
// projectconfig.
const projectConfigFile = ".makeslop.yaml"

func (p Project) containerPath() string {
	return "/workspace/" + p.Name
}

func projectSandbox(p Project) []Mount {
	var mounts []Mount
	workspacePath := p.containerPath()
	if p.ProtectConfig {
		mounts = append(mounts, Mount{
			Host:      filepath.Join(p.Host, projectConfigFile),
			Container: workspacePath + "/" + projectConfigFile,
			ReadOnly:  true,
		})
	}
	if p.MaskGitHooks {
		mounts = append(mounts, Mount{
			Type:      "tmpfs",
			Container: workspacePath + "/.git/hooks",
		})
	}
	return mounts
}

// A config mask would override the read-only policy bind because Docker applies
// later mounts last.
func projectMasks(p Project, configBound bool) []Mount {
	workspacePath := p.containerPath()
	maskedFiles := p.MaskedFiles
	if configBound {
		maskedFiles = filterOut(maskedFiles, filepath.Join(p.Host, projectConfigFile))
	}

	// Validated mask paths are beneath Host, so Rel cannot fail on POSIX.
	var mounts []Mount
	for _, host := range maskedFiles {
		rel, _ := filepath.Rel(p.Host, host)
		mounts = append(mounts, Mount{
			Host:      "/dev/null",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}
	for _, host := range p.MaskedDirs {
		rel, _ := filepath.Rel(p.Host, host)
		mounts = append(mounts, Mount{
			Type:      "tmpfs",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}
	return mounts
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
	if s.NetworkMode != "" {
		args = append(args, "--network", s.NetworkMode)
	}
	for _, n := range s.Networks {
		args = append(args, "--network", n)
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

// Keep each label on one comment line, even for paths containing newlines.
func sanitizeLabel(label string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, label)
}

// ShellCommand renders a readable docker command. Section comments break shell
// continuation, so output with joins is not pasteable as one command.
func (s Spec) ShellCommand() string {
	args := s.Args() // starts with "run", not "docker"

	var lines []string
	lines = append(lines, "docker run")
	annotations := make(map[int]bool)

	// Each mount emits one --mount token, so section indices match mount ordinals.
	sections := make(map[int]string, len(s.Sections))
	for _, sec := range s.Sections {
		sections[sec.Start] = sec.Label
	}
	mountN := 0

	i := 1 // skip "run" — already in "docker run" prefix
	for i < len(args)-2 {
		tok := args[i]
		if tok == "--mount" {
			if label, ok := sections[mountN]; ok {
				annotations[len(lines)] = true
				lines = append(lines, "")
				annotations[len(lines)] = true
				lines = append(lines, "  # --- "+sanitizeLabel(label)+" ---")
			}
			mountN++
		}
		switch tok {
		case "--workdir", "--tmpfs", "--cap-drop", "--security-opt", "--network", "--mount", "-e":
			lines = append(lines, "  "+shellQuote(tok)+" "+shellQuote(args[i+1]))
			i += 2
		default:
			lines = append(lines, "  "+shellQuote(tok))
			i++
		}
	}
	if len(s.Sections) > 0 {
		// Close the last project group off from the image/command tail.
		annotations[len(lines)] = true
		lines = append(lines, "")
	}
	// Explicit tail lines: a flag-shaped image name must not be parsed as a flag.
	lines = append(lines, "  "+shellQuote(args[len(args)-2]))
	lines = append(lines, "  "+shellQuote(args[len(args)-1]))

	var sb strings.Builder
	for j, line := range lines {
		sb.WriteString(line)
		if j < len(lines)-1 && !annotations[j] {
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
		NetworkMode: container.NetworkMode(s.primaryNetwork()),
	}
}

// primaryNetwork is the value for HostConfig.NetworkMode: NetworkMode when set,
// else the first of Networks, else "" (Docker default).
func (s Spec) primaryNetwork() string {
	if s.NetworkMode != "" {
		return s.NetworkMode
	}
	if len(s.Networks) > 0 {
		return s.Networks[0]
	}
	return ""
}

// NetworkingConfig returns the SDK endpoint config attaching every entry of
// Networks at create time, or nil when Networks is empty (NetworkMode alone
// is carried by HostConfig). Multiple endpoints need daemon API >= 1.44.
func (s Spec) NetworkingConfig() *network.NetworkingConfig {
	if len(s.Networks) == 0 {
		return nil
	}
	eps := make(map[string]*network.EndpointSettings, len(s.Networks))
	for _, n := range s.Networks {
		eps[n] = &network.EndpointSettings{}
	}
	return &network.NetworkingConfig{EndpointsConfig: eps}
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

// csvField returns s as a single RFC 4180 CSV field: unquoted when free of
// CSV-special characters, otherwise wrapped in `"` with embedded `"` doubled.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

package docker

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

// ProjectRoot pairs a resolved absolute host root with its container destination.
type ProjectRoot struct {
	Name, HostRoot, ContainerRoot string
}

type Project struct {
	ProjectRoot
	Label    string
	ReadOnly bool // ignored for the main project

	MaskedFiles []string
	MaskedDirs  []string

	// Set only for an existing file; a missing bind source fails container creation.
	ProtectConfig bool

	// Only directory-backed .git can have hooks masked within this mount.
	MaskGitHooks bool
}

// Layout contains the writable main project and its joins in flag order.
type Layout struct {
	Main  Project
	Joins []Project
}

// Validate requires the same resolved roots for target checks and mount creation.
func (l Layout) Validate() error {
	if err := l.Main.validateRoots(); err != nil {
		return fmt.Errorf("main project: %w", err)
	}

	for i, join := range l.Joins {
		if err := join.validateRoots(); err != nil {
			return fmt.Errorf("join[%d]: %w", i, err)
		}
	}

	return nil
}

func (r ProjectRoot) validateRoots() error {
	if !filepath.IsAbs(r.HostRoot) {
		return fmt.Errorf("host root %q must be absolute", r.HostRoot)
	}

	if !path.IsAbs(r.ContainerRoot) || path.Clean(r.ContainerRoot) != r.ContainerRoot {
		return fmt.Errorf("container root %q must be clean and absolute", r.ContainerRoot)
	}

	return nil
}

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

// Mount keeps the same representation for CLI and SDK projections.
type Mount struct {
	Type            MountKind
	Host, Container string
	ReadOnly        bool
}

type MountKind string

const (
	MountBind   MountKind = "bind"
	MountTmpfs  MountKind = "tmpfs"
	MountVolume MountKind = "volume"
)

type section struct {
	Label string
	Start int // index into createSpec.Mounts
}

func projectBind(p Project) Mount {
	return Mount{Type: MountBind, Host: p.HostRoot, Container: p.ContainerRoot, ReadOnly: p.ReadOnly}
}

func projectSandbox(p Project) []Mount {
	var mounts []Mount
	workspacePath := p.ContainerRoot
	if p.ProtectConfig {
		mounts = append(mounts, Mount{
			Type:      MountBind,
			Host:      filepath.Join(p.HostRoot, projectconfig.Filename),
			Container: workspacePath + "/" + projectconfig.Filename,
			ReadOnly:  true,
		})
	}
	if p.MaskGitHooks {
		mounts = append(mounts, Mount{
			Type:      MountTmpfs,
			Container: workspacePath + "/.git/hooks",
		})
	}
	return mounts
}

// A config mask would override the read-only policy bind because Docker applies
// later mounts last.
func projectMasks(p Project, configBound bool) []Mount {
	workspacePath := p.ContainerRoot
	maskedFiles := p.MaskedFiles
	if configBound {
		maskedFiles = filterOut(maskedFiles, filepath.Join(p.HostRoot, projectconfig.Filename))
	}

	// Validated mask paths are beneath Host, so Rel cannot fail on POSIX.
	var mounts []Mount
	for _, host := range maskedFiles {
		rel, _ := filepath.Rel(p.HostRoot, host)
		mounts = append(mounts, Mount{
			Type:      MountBind,
			Host:      "/dev/null",
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}
	for _, host := range p.MaskedDirs {
		rel, _ := filepath.Rel(p.HostRoot, host)
		mounts = append(mounts, Mount{
			Type:      MountTmpfs,
			Container: workspacePath + "/" + filepath.ToSlash(rel),
		})
	}
	return mounts
}

// Args uses CSV quoting so Docker can parse mount paths containing ',' or '"'.
func (s createSpec) Args() []string {
	args := []string{"run"}

	if s.AutoRemove {
		args = append(args, "--rm")
	}

	flags := "-"

	if !s.Attached {
		flags += "d"
	}

	if s.OpenStdin {
		flags += "i"
	}

	if s.Tty {
		flags += "t"
	}

	if flags != "-" {
		args = append(args, flags)
	}

	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}

	for _, l := range s.Labels {
		args = append(args, "--label", l.key+"="+l.value)
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
		case MountTmpfs:
			args = append(args, "--mount", "type=tmpfs,"+csvField("target="+m.Container))

		case MountVolume:
			val := "type=volume," + csvField("source="+m.Host) + "," + csvField("target="+m.Container)

			if m.ReadOnly {
				val += ",readonly"
			}

			args = append(args, "--mount", val)

		case MountBind:
			val := "type=bind," + csvField("source="+m.Host) + "," + csvField("target="+m.Container)

			if m.ReadOnly {
				val += ",readonly"
			}

			args = append(args, "--mount", val)
		}
	}

	args = append(args, s.Image)

	return append(args, s.Argv...)
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

// ShellCommand labels join sections for inspection. With joins, those labels
// break shell continuation, so the result cannot be pasted as one command.
func (s createSpec) ShellCommand() string {
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

	tail := len(args) - 1 - len(s.Argv) // image and final argv
	i := 1                              // skip "run" — already in "docker run" prefix
	for i < tail {
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
		case "--workdir", "--tmpfs", "--cap-drop", "--security-opt", "--network", "--mount", "-e", "--name", "--label":
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
	for _, arg := range args[tail:] {
		lines = append(lines, "  "+shellQuote(arg))
	}

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

func (s createSpec) ContainerConfig() *container.Config {
	cfg := &container.Config{
		Image:        s.Image,
		Cmd:          slices.Clone(s.Argv),
		WorkingDir:   s.Workdir,
		Env:          slices.Clone(s.Env),
		Tty:          s.Tty,
		OpenStdin:    s.OpenStdin,
		AttachStdin:  s.Attached,
		AttachStdout: s.Attached,
		AttachStderr: s.Attached,
	}
	if len(s.Labels) > 0 {
		cfg.Labels = make(map[string]string, len(s.Labels))
		for _, l := range s.Labels {
			cfg.Labels[l.key] = l.value
		}
	}
	return cfg
}

func (s createSpec) HostConfig() *container.HostConfig {
	return &container.HostConfig{
		AutoRemove:  s.AutoRemove,
		CapDrop:     slices.Clone(s.CapDrop),
		SecurityOpt: slices.Clone(s.SecOpt),
		Tmpfs:       tmpfsMap(s.Tmpfs),
		Mounts:      mountsFor(s.Mounts),
		NetworkMode: container.NetworkMode(s.primaryNetwork()),
	}
}

// primaryNetwork is the value for HostConfig.NetworkMode: NetworkMode when set,
// else the first of Networks, else "" (Docker default).
func (s createSpec) primaryNetwork() string {
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
func (s createSpec) NetworkingConfig() *network.NetworkingConfig {
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

func mountsFor(mounts []Mount) []mount.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]mount.Mount, len(mounts))
	for i, m := range mounts {
		switch m.Type {
		case MountTmpfs:
			out[i] = mount.Mount{
				Type:   mount.TypeTmpfs,
				Target: m.Container,
			}
		case MountVolume:
			// Host carries the Docker volume name for volume mounts.
			out[i] = mount.Mount{
				Type:     mount.TypeVolume,
				Source:   m.Host,
				Target:   m.Container,
				ReadOnly: m.ReadOnly,
			}
		case MountBind:
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

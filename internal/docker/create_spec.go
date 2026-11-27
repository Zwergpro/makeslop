package docker

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	moby "github.com/moby/moby/client"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

// BuildInputs contains resolved launch values; the builders perform no I/O.
type BuildInputs struct {
	Layout      Layout
	Image       string
	Shell       string
	TmpDirSize  string
	Env         []string
	Network     projectconfig.Network
	CacheMounts []Mount
}

type label struct{ key, value string }

// createSpec holds final values shared by the shell and SDK projections.
type createSpec struct {
	Image, Workdir, Name   string
	Argv, Env              []string
	Labels                 []label
	Attached, AutoRemove   bool
	Tty, OpenStdin         bool
	Mounts                 []Mount
	Tmpfs, CapDrop, SecOpt []string
	NetworkMode            string
	Networks               []string
	Sections               []section // Display only.
}

type ForegroundSpec struct{ create createSpec }

type BackgroundSpec struct {
	create createSpec
	uuid   string // Public ID used for readiness and lifecycle diagnostics.
}

func BuildForeground(in BuildInputs) (ForegroundSpec, error) {
	s, err := buildCreateSpec(in)
	if err != nil {
		return ForegroundSpec{}, err
	}
	s.Argv = []string{in.Shell}
	s.Env = slices.Clone(in.Env)
	s.Attached = true
	s.AutoRemove = true

	return ForegroundSpec{create: s}, nil
}

func BuildBackground(in BuildInputs, uuid string) (BackgroundSpec, error) {
	if !validBGUUID(uuid) {
		return BackgroundSpec{}, fmt.Errorf("expected a full background UUID")
	}
	s, err := buildCreateSpec(in)
	if err != nil {
		return BackgroundSpec{}, err
	}
	s.Name = bgPrefix + uuid
	s.Labels = []label{{bgUUIDLabel, uuid}, {bgProjectLabel, in.Layout.Main.Name}, {bgShellLabel, in.Shell}}
	s.Argv = []string{"zmx", "attach", bgSession, in.Shell}
	s.Env = backgroundEnv(in.Env)

	return BackgroundSpec{create: s, uuid: uuid}, nil
}

func backgroundEnv(entries []string) []string {
	env := make([]string, 0, len(entries)+len(bgZMXEnv))
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ZMX_DIR", "ZMX_DIR_MODE", "ZMX_SESSION", "ZMX_SESSION_PREFIX":
		default:
			env = append(env, entry)
		}
	}

	return append(env, bgZMXEnv...)
}

func (s ForegroundSpec) ShellCommand() string { return s.create.ShellCommand() }

func (s BackgroundSpec) ShellCommand() string { return s.create.ShellCommand() }

func (s createSpec) createOptions() moby.ContainerCreateOptions {
	return moby.ContainerCreateOptions{
		Name:             s.Name,
		Config:           s.ContainerConfig(),
		HostConfig:       s.HostConfig(),
		NetworkingConfig: s.NetworkingConfig(),
	}
}

// Mount order is significant: later masks shadow earlier binds.
func buildCreateSpec(o BuildInputs) (createSpec, error) {
	if !filepath.IsAbs(o.Layout.Main.HostRoot) || !path.IsAbs(o.Layout.Main.ContainerRoot) {
		return createSpec{}, fmt.Errorf("main project requires absolute host and container roots")
	}
	for _, m := range o.CacheMounts {
		switch m.Type {
		case MountBind, MountTmpfs, MountVolume:
		default:
			return createSpec{}, fmt.Errorf("unknown mount kind %q", m.Type)
		}
	}

	main := o.Layout.Main
	main.ReadOnly = false
	workspacePath := main.ContainerRoot

	// Directory trailing slashes make Docker reject unexpected files early.
	mounts := []Mount{projectBind(main)}
	// The policy mounts must shadow the writable project bind.
	mounts = append(mounts, projectSandbox(main)...)

	mounts = append(mounts, o.CacheMounts...)

	mounts = append(mounts, projectMasks(main, main.ProtectConfig)...)

	// An ro join needs no policy self-bind, so its config mask remains effective.
	var sections []section
	if len(o.Layout.Joins) > 0 {
		sections = append(sections, section{Label: main.Label, Start: 0})
	}
	for _, j := range o.Layout.Joins {
		sections = append(sections, section{Label: j.Label, Start: len(mounts)})
		mounts = append(mounts, projectBind(j))
		if !j.ReadOnly {
			mounts = append(mounts, projectSandbox(j)...)
		}
		mounts = append(mounts, projectMasks(j, !j.ReadOnly && j.ProtectConfig)...)
	}

	return createSpec{
		Image:     o.Image,
		Workdir:   workspacePath,
		Tty:       true,
		OpenStdin: true,
		Mounts:    mounts,
		Tmpfs:     []string{"/tmp:size=" + o.TmpDirSize},
		CapDrop:   []string{"ALL"},
		SecOpt:    []string{"no-new-privileges"},

		NetworkMode: o.Network.Mode,
		Networks:    slices.Clone(o.Network.Networks),

		Sections: sections,
	}, nil
}

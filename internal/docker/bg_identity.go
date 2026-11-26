package docker

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

const (
	bgPrefix       = "makeslop-"
	bgUUIDLabel    = "io.makeslop.bg.uuid"
	bgProjectLabel = "io.makeslop.bg.project"
	bgShellLabel   = "io.makeslop.bg.shell"
)

var bgUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewBGUUID generates the public handle for a new background container.
func NewBGUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate background UUID: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// BGContainer describes a background container discovered on the selected daemon.
// ID is the internal daemon ID, not the public UUID used by the CLI.
type BGContainer struct {
	UUID, ID, Project, State, Image, Shell, Workdir string
	Created                                         int64
	user                                            string
	running                                         bool
	stopped                                         bool
}

func bgIdentity(name string, labels map[string]string) (string, bool) {
	uuid, ok := strings.CutPrefix(strings.TrimPrefix(name, "/"), bgPrefix)
	return uuid, ok && bgUUIDPattern.MatchString(uuid) && labels[bgUUIDLabel] == uuid
}

// ListBG returns managed containers in newest-first order, including stopped ones.
func (d *Docker) ListBG(ctx context.Context) ([]BGContainer, error) {
	reply, err := d.client.ContainerList(ctx, moby.ContainerListOptions{All: true, Filters: moby.Filters{}.Add("name", "^/"+bgPrefix)})
	if err != nil {
		return nil, fmt.Errorf("list background containers: %w", err)
	}
	var rows []BGContainer
	for _, item := range reply.Items {
		for _, name := range item.Names {
			uuid, ok := bgIdentity(name, item.Labels)
			if !ok {
				continue
			}
			rows = append(rows, BGContainer{UUID: uuid, ID: item.ID, Project: item.Labels[bgProjectLabel], Image: item.Image, State: string(item.State), Created: item.Created})
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Created == rows[j].Created {
			return rows[i].UUID < rows[j].UUID
		}
		return rows[i].Created > rows[j].Created
	})
	return rows, nil
}

func (d *Docker) lookupBG(ctx context.Context, uuid string) (BGContainer, error) {
	if !bgUUIDPattern.MatchString(uuid) {
		return BGContainer{}, fmt.Errorf("expected a full background UUID")
	}
	name := bgPrefix + uuid
	reply, err := d.client.ContainerInspect(ctx, name, moby.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BGContainer{}, fmt.Errorf("background container %s not found", uuid)
		}
		return BGContainer{}, bgDaemonError("inspect", uuid, err)
	}
	c := reply.Container
	if c.Config != nil && c.ID != "" {
		if id, ok := bgIdentity(c.Name, c.Config.Labels); ok && id == uuid {
			row := BGContainer{UUID: uuid, ID: c.ID, Project: c.Config.Labels[bgProjectLabel], Image: c.Config.Image, Shell: c.Config.Labels[bgShellLabel], Workdir: c.Config.WorkingDir, user: c.Config.User}
			if st := c.State; st != nil {
				row.State = string(st.Status)
				row.running = st.Running && !st.Paused && !st.Restarting && st.Status == "running"
				row.stopped = !st.Running && !st.Paused && !st.Restarting && (st.Status == "created" || st.Status == "exited" || st.Status == "dead")
			}
			return row, nil
		}
	}
	return BGContainer{}, fmt.Errorf("background container %s not found", uuid)
}

func bgDaemonError(action, uuid string, err error) error {
	if strings.Contains(err.Error(), bgPrefix+uuid) {
		return fmt.Errorf("%s background container %s: %s", action, uuid, strings.ReplaceAll(err.Error(), bgPrefix+uuid, uuid))
	}
	return fmt.Errorf("%s background container %s: %w", action, uuid, err)
}

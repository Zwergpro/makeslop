package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"uuid"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

const (
	bgPrefix       = "makeslop-"
	bgUUIDLabel    = "io.makeslop.bg.uuid"
	bgProjectLabel = "io.makeslop.bg.project"
	bgShellLabel   = "io.makeslop.bg.shell"
)

func validBGUUID(s string) bool {
	u, err := uuid.Parse(s)
	if err != nil {
		return false
	}

	// Parse accepts alternate spellings; Docker names use only canonical v4 UUIDs.
	isV4 := u[6]>>4 == 4 && u[8]>>6 == 0b10

	return isV4 && s == u.String()
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

	return uuid, ok && validBGUUID(uuid) && labels[bgUUIDLabel] == uuid
}

// ListBG returns managed containers in newest-first order, including stopped ones.
func (d *Docker) ListBG(ctx context.Context) ([]BGContainer, error) {
	reply, err := d.client.ContainerList(
		ctx,
		moby.ContainerListOptions{All: true, Filters: moby.Filters{}.Add("name", "^/"+bgPrefix)},
	)
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

			rows = append(
				rows,
				BGContainer{
					UUID:    uuid,
					ID:      item.ID,
					Project: item.Labels[bgProjectLabel],
					Image:   item.Image,
					State:   string(item.State),
					Created: item.Created,
				},
			)

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
	if !validBGUUID(uuid) {
		return BGContainer{}, fmt.Errorf("expected a full background UUID")
	}

	name := bgPrefix + uuid
	lookupCtx, cancel := WithPreflightTimeout(ctx)
	defer cancel()
	reply, err := d.client.ContainerInspect(lookupCtx, name, moby.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BGContainer{}, fmt.Errorf("background container %s not found", uuid)
		}

		return BGContainer{}, bgDaemonError("inspect", uuid, err)
	}

	c := reply.Container
	if c.Config != nil && c.ID != "" {
		if id, ok := bgIdentity(c.Name, c.Config.Labels); ok && id == uuid {
			row := BGContainer{
				UUID:    uuid,
				ID:      c.ID,
				Project: c.Config.Labels[bgProjectLabel],
				Image:   c.Config.Image,
				Shell:   c.Config.Labels[bgShellLabel],
				Workdir: c.Config.WorkingDir,
				user:    c.Config.User,
			}

			if st := c.State; st != nil {
				row.State = string(st.Status)
				stable := !st.Paused && !st.Restarting
				stoppedState := st.Status == "created" || st.Status == "exited" || st.Status == "dead"

				row.running = stable && st.Running && st.Status == "running"
				row.stopped = stable && !st.Running && stoppedState
			}

			return row, nil
		}
	}

	return BGContainer{}, fmt.Errorf("background container %s not found", uuid)
}

func bgDaemonError(action, uuid string, err error) error {
	if strings.Contains(err.Error(), bgPrefix+uuid) {
		return fmt.Errorf(
			"%s background container %s: %s",
			action,
			uuid,
			strings.ReplaceAll(err.Error(), bgPrefix+uuid, uuid),
		)
	}

	return fmt.Errorf("%s background container %s: %w", action, uuid, err)
}

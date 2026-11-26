package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

const testBGUUID = "018009ac-5fec-4c37-891a-244c91beda77"

const secondBGUUID = "118009ac-5fec-4c37-a91a-244c91beda77"

func bgLabels(uuid string) map[string]string {
	return map[string]string{
		"io.makeslop.bg.uuid":    uuid,
		"io.makeslop.bg.project": "demo",
		"io.makeslop.bg.shell":   "/bin/zsh",
	}
}

func bgInspection(uuid string, state container.ContainerState) moby.ContainerInspectResult {
	return moby.ContainerInspectResult{Container: container.InspectResponse{
		ID:    "daemon-id",
		Name:  "/makeslop-" + uuid,
		State: &container.State{Status: state, Running: state == "running"},
		Config: &container.Config{
			Labels:     bgLabels(uuid),
			Image:      "local:dev",
			User:       "1000",
			WorkingDir: "/workspace/demo",
		},
	}}
}

type bgIdentityClient struct {
	noopClient
	inspected   string
	result      moby.ContainerInspectResult
	inspectErr  error
	listOptions moby.ContainerListOptions
	items       []container.Summary
}

func (f *bgIdentityClient) ContainerInspect(
	_ context.Context,
	name string,
	_ moby.ContainerInspectOptions,
) (moby.ContainerInspectResult, error) {
	f.inspected = name

	return f.result, f.inspectErr
}

func (f *bgIdentityClient) ContainerList(
	_ context.Context,
	opts moby.ContainerListOptions,
) (moby.ContainerListResult, error) {
	f.listOptions = opts

	return moby.ContainerListResult{Items: f.items}, nil
}

func TestBGIdentity(t *testing.T) {
	for _, uuid := range []string{
		"018009ac", "makeslop-" + testBGUUID, strings.ToUpper(testBGUUID), "garbage",
		strings.ReplaceAll(testBGUUID, "-", ""), "urn:uuid:" + testBGUUID, "{" + testBGUUID + "}",
		"018009ac-5fec-7c37-891a-244c91beda77", // not v4
		"018009ac-5fec-4c37-c91a-244c91beda77", // not RFC 9562 variant
	} {
		t.Run(uuid, func(t *testing.T) {
			f := &bgIdentityClient{result: bgInspection(testBGUUID, "running")}
			d := &Docker{client: f}

			if _, err := d.lookupBG(t.Context(), uuid); err == nil || f.inspected != "" {
				t.Fatalf("invalid handle inspected %q, err %v", f.inspected, err)
			}
		})
	}

	f := &bgIdentityClient{result: bgInspection(testBGUUID, "running")}
	d := &Docker{client: f}
	got, err := d.lookupBG(t.Context(), testBGUUID)
	if err != nil || got.ID != "daemon-id" || f.inspected != "makeslop-"+testBGUUID {
		t.Fatalf("lookup: %+v, %v, query %q", got, err, f.inspected)
	}

	f.result.Container.Name = "/renamed"

	if got, err := d.lookupBG(t.Context(), testBGUUID); err == nil || got.ID != "" {
		t.Fatalf("renamed container accepted: %+v, %v", got, err)
	}

	f.result = bgInspection(testBGUUID, "running")
	delete(f.result.Container.Config.Labels, "io.makeslop.bg.uuid")

	if got, err := d.lookupBG(t.Context(), testBGUUID); err == nil || got.ID != "" {
		t.Fatalf("unlabelled container accepted: %+v, %v", got, err)
	}

	failure := errors.New("daemon offline")
	f.inspectErr = failure

	if _, err := d.lookupBG(t.Context(), testBGUUID); !errors.Is(err, failure) {
		t.Fatalf("daemon error: %v", err)
	}

	f.inspectErr = errdefs.ErrNotFound.WithMessage("no such container: makeslop-" + testBGUUID)

	if _, err := d.lookupBG(t.Context(), testBGUUID); err == nil || strings.Contains(err.Error(), "makeslop-") {
		t.Fatalf("not-found error: %v", err)
	}
}

func TestListBG(t *testing.T) {
	f := &bgIdentityClient{items: []container.Summary{
		{
			ID:      "second",
			Names:   []string{"/makeslop-" + secondBGUUID},
			Labels:  bgLabels(secondBGUUID),
			Created: 2,
			State:   "running",
		},
		{ID: "lookalike", Names: []string{"/makeslop-" + testBGUUID}, Labels: bgLabels(secondBGUUID), Created: 3},
		{ID: "substring", Names: []string{"/prefix-makeslop-" + testBGUUID}, Labels: bgLabels(testBGUUID), Created: 4},
		{
			ID:      "first",
			Names:   []string{"/makeslop-" + testBGUUID},
			Labels:  bgLabels(testBGUUID),
			Created: 2,
			State:   "exited",
		},
	}}
	d := &Docker{client: f}
	got, err := d.ListBG(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if !f.listOptions.All {
		t.Fatal("stopped containers not requested")
	}

	var ids []string

	for _, row := range got {
		ids = append(ids, row.ID)
	}

	if !slices.Equal(ids, []string{"first", "second"}) {
		t.Fatalf("listed %v", ids)
	}

	if got[0].State != "exited" || got[0].UUID != testBGUUID || got[0].Project != "demo" {
		t.Fatalf("stopped row %+v", got[0])
	}
}

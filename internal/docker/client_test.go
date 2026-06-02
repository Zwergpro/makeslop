package docker

import (
	"context"
	"errors"
	"io"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// TestAPIClientSatisfied verifies at compile time (and at test time) that
// *moby.Client satisfies our narrow apiClient interface. The compile-time
// assertion in client.go is the real guard; this test makes the fact explicit
// and visible in test output.
func TestAPIClientSatisfied(t *testing.T) {
	// The compile-time assertion `var _ apiClient = (*moby.Client)(nil)` in
	// client.go already enforces this. The test below is redundant in that
	// sense but documents the intent and fails fast at runtime if the build
	// constraint was somehow bypassed.
	var c interface{} = (*moby.Client)(nil)
	if _, ok := c.(apiClient); !ok {
		t.Fatal("*moby.Client does not satisfy apiClient interface")
	}
}

// TestNewClientReturnsNonNil verifies that newClient() returns a non-nil
// client when called without a real daemon. The connection is lazy so this
// works even when DOCKER_HOST is unset or points at a non-existent socket.
func TestNewClientReturnsNonNil(t *testing.T) {
	c, err := newClient()
	if err != nil {
		t.Fatalf("newClient() returned error: %v", err)
	}
	if c == nil {
		t.Fatal("newClient() returned nil client")
	}
	// Close is a no-op when no connection has been established.
	_ = c.Close()
}

// TestNoopClientPingOK verifies the noopClient Ping returns success.
func TestNoopClientPingOK(t *testing.T) {
	var n noopClient
	_, err := n.Ping(context.Background(), moby.PingOptions{})
	if err != nil {
		t.Fatalf("noopClient.Ping() returned error: %v", err)
	}
}

// TestNoopClientImageInspectFound verifies the noopClient ImageInspect returns
// a result (image "found") with no error.
func TestNoopClientImageInspectFound(t *testing.T) {
	var n noopClient
	_, err := n.ImageInspect(context.Background(), "myimage")
	if err != nil {
		t.Fatalf("noopClient.ImageInspect() returned error: %v", err)
	}
}

// TestFakeRunClientPingScripting verifies that FakeRunClient returns the
// scripted PingErr when set, and success otherwise.
func TestFakeRunClientPingScripting(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := NewFakeRunClient(0)
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err != nil {
			t.Fatalf("expected ping success, got: %v", err)
		}
	})
	t.Run("daemon-down", func(t *testing.T) {
		f := NewFakeRunClient(0)
		f.PingErr = errors.New("connection refused")
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err == nil {
			t.Fatal("expected ping error, got nil")
		}
	})
}

// TestFakeRunClientImageInspectScripting verifies the image-present,
// image-missing, and other-error cases.
func TestFakeRunClientImageInspectScripting(t *testing.T) {
	t.Run("image-present", func(t *testing.T) {
		f := NewFakeRunClient(0)
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err != nil {
			t.Fatalf("expected image found, got: %v", err)
		}
	})
	t.Run("image-missing returns not-found", func(t *testing.T) {
		f := NewFakeRunClient(0)
		f.ImageMissing = true
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !cerrdefs.IsNotFound(err) {
			t.Fatalf("expected cerrdefs.IsNotFound to be true, got error: %v", err)
		}
	})
	t.Run("image other-error propagates", func(t *testing.T) {
		f := NewFakeRunClient(0)
		f.ImageErr = errors.New("permission denied")
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if cerrdefs.IsNotFound(err) {
			t.Fatal("other-error should NOT be classified as not-found")
		}
	})
}

// TestFakeBuildClientPingScripting verifies FakeBuildClient ping scripting.
func TestFakeBuildClientPingScripting(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err != nil {
			t.Fatalf("expected ping success, got: %v", err)
		}
	})
	t.Run("daemon-down", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		f.PingErr = errors.New("connection refused")
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err == nil {
			t.Fatal("expected ping error, got nil")
		}
	})
}

// TestFakeBuildClientImageInspectScripting verifies image scripting on
// FakeBuildClient: present, missing (not-found), and other-error cases.
func TestFakeBuildClientImageInspectScripting(t *testing.T) {
	t.Run("image-present", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err != nil {
			t.Fatalf("expected image found, got: %v", err)
		}
	})
	t.Run("image-missing returns not-found", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		f.ImageMissing = true
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !cerrdefs.IsNotFound(err) {
			t.Fatalf("expected cerrdefs.IsNotFound to be true, got error: %v", err)
		}
	})
	t.Run("image other-error propagates", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		f.ImageErr = errors.New("permission denied")
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if cerrdefs.IsNotFound(err) {
			t.Fatal("other-error should NOT be classified as not-found")
		}
	})
}

// ─── FakeRunClient volume tracking ───────────────────────────────────────────

// TestFakeRunClient_VolumeCreate records the created volume name.
func TestFakeRunClient_VolumeCreate(t *testing.T) {
	f := NewFakeRunClient(0)
	result, err := f.VolumeCreate(context.Background(), moby.VolumeCreateOptions{Name: "my-volume"})
	if err != nil {
		t.Fatalf("VolumeCreate returned error: %v", err)
	}
	if result.Volume.Name != "my-volume" {
		t.Errorf("VolumeCreate result.Volume.Name = %q, want %q", result.Volume.Name, "my-volume")
	}
	if len(f.CreatedVolumes) != 1 || f.CreatedVolumes[0] != "my-volume" {
		t.Errorf("CreatedVolumes = %v, want [my-volume]", f.CreatedVolumes)
	}
}

// TestFakeRunClient_VolumeRemove records the removed volume name.
func TestFakeRunClient_VolumeRemove(t *testing.T) {
	f := NewFakeRunClient(0)
	_, err := f.VolumeRemove(context.Background(), "my-volume", moby.VolumeRemoveOptions{})
	if err != nil {
		t.Fatalf("VolumeRemove returned error: %v", err)
	}
	if len(f.RemovedVolumes) != 1 || f.RemovedVolumes[0] != "my-volume" {
		t.Errorf("RemovedVolumes = %v, want [my-volume]", f.RemovedVolumes)
	}
}

// TestFakeRunClient_VolumeCreateRemove_RecordBoth verifies that create followed
// by remove is tracked in order (matching the production teardown pattern).
func TestFakeRunClient_VolumeCreateRemove_RecordBoth(t *testing.T) {
	f := NewFakeRunClient(0)
	_, _ = f.VolumeCreate(context.Background(), moby.VolumeCreateOptions{Name: "vol-a"})
	_, _ = f.VolumeCreate(context.Background(), moby.VolumeCreateOptions{Name: "vol-b"})
	_, _ = f.VolumeRemove(context.Background(), "vol-b", moby.VolumeRemoveOptions{})
	_, _ = f.VolumeRemove(context.Background(), "vol-a", moby.VolumeRemoveOptions{})

	if len(f.CreatedVolumes) != 2 {
		t.Errorf("CreatedVolumes len = %d, want 2", len(f.CreatedVolumes))
	}
	if len(f.RemovedVolumes) != 2 {
		t.Errorf("RemovedVolumes len = %d, want 2", len(f.RemovedVolumes))
	}
	// Teardown order: reverse of creation (b first, then a)
	if f.RemovedVolumes[0] != "vol-b" || f.RemovedVolumes[1] != "vol-a" {
		t.Errorf("RemovedVolumes = %v, want [vol-b vol-a]", f.RemovedVolumes)
	}
}

// ─── FakeRunClient exec handshake (create→attach→inspect) ────────────────────

// TestFakeRunClient_ExecHandshake_Success verifies the happy-path exec handshake:
// ExecCreate → ExecAttach (blocks until exec exits) → ExecInspect returns exit code 0.
func TestFakeRunClient_ExecHandshake_Success(t *testing.T) {
	f := NewFakeRunClient(0)
	f.ExecExitCode = 0 // explicit: success

	execID, err := f.ExecCreate(context.Background(), "container-id", moby.ExecCreateOptions{
		Cmd: []string{"test", "-S", "/sockets/proxy.sock"},
	})
	if err != nil {
		t.Fatalf("ExecCreate returned error: %v", err)
	}
	if execID.ID == "" {
		t.Fatal("ExecCreate returned empty exec ID")
	}

	attachRes, err := f.ExecAttach(context.Background(), execID.ID, moby.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("ExecAttach returned error: %v", err)
	}
	// Drain to EOF — blocks until exec exits (fake closes pipe immediately).
	_, _ = io.Copy(io.Discard, attachRes.Reader)
	attachRes.Close()

	result, err := f.ExecInspect(context.Background(), execID.ID, moby.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("ExecInspect returned error: %v", err)
	}
	if result.Running {
		t.Error("ExecInspect: Running = true, want false (exec completed)")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExecInspect: ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestFakeRunClient_ExecHandshake_Running verifies ExecInspect reports Running=true
// when ExecRunning is set (simulating an in-progress exec).
func TestFakeRunClient_ExecHandshake_Running(t *testing.T) {
	f := NewFakeRunClient(0)
	f.ExecRunning = true

	_, err := f.ExecCreate(context.Background(), "container-id", moby.ExecCreateOptions{})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}
	result, err := f.ExecInspect(context.Background(), "fake-exec-id", moby.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}
	if !result.Running {
		t.Error("ExecInspect: Running = false, want true")
	}
}

// TestFakeRunClient_ExecHandshake_NonZeroExit verifies ExecInspect returns the
// scripted non-zero exit code.
func TestFakeRunClient_ExecHandshake_NonZeroExit(t *testing.T) {
	f := NewFakeRunClient(0)
	f.ExecExitCode = 1

	result, err := f.ExecInspect(context.Background(), "fake-exec-id", moby.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("ExecInspect: ExitCode = %d, want 1", result.ExitCode)
	}
}

// ─── FakeRunClient sidecar early-exit simulation ─────────────────────────────

// TestFakeRunClient_ContainerInspect_Running verifies that ContainerInspect
// reports a running state by default.
func TestFakeRunClient_ContainerInspect_Running(t *testing.T) {
	f := NewFakeRunClient(0)
	result, err := f.ContainerInspect(context.Background(), "container-id", moby.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if result.Container.State == nil {
		t.Fatal("ContainerInspect: State is nil")
	}
	if !result.Container.State.Running {
		t.Error("ContainerInspect: State.Running = false, want true")
	}
}

// TestFakeRunClient_ContainerInspect_SidecarExited verifies that SidecarExited
// causes ContainerInspect to report a non-running exited state.
func TestFakeRunClient_ContainerInspect_SidecarExited(t *testing.T) {
	f := NewFakeRunClient(0)
	f.SidecarExited = true

	result, err := f.ContainerInspect(context.Background(), "container-id", moby.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if result.Container.State == nil {
		t.Fatal("ContainerInspect: State is nil")
	}
	if result.Container.State.Running {
		t.Error("ContainerInspect: State.Running = true, want false (sidecar exited)")
	}
	if result.Container.State.ExitCode == 0 {
		t.Error("ContainerInspect: State.ExitCode = 0, want non-zero (sidecar exited with error)")
	}
}

// ─── FakeRunClient image-pull simulation ─────────────────────────────────────

// TestFakeRunClient_ImagePull_Success verifies that ImagePull records the call
// and returns a valid (non-nil, non-error) response when ImagePullErr is nil.
func TestFakeRunClient_ImagePull_Success(t *testing.T) {
	f := NewFakeRunClient(0)
	resp, err := f.ImagePull(context.Background(), "alpine/socat:latest", moby.ImagePullOptions{})
	if err != nil {
		t.Fatalf("ImagePull returned error: %v", err)
	}
	if !f.ImagePullCalled {
		t.Error("ImagePullCalled = false after ImagePull")
	}
	if resp == nil {
		t.Fatal("ImagePull returned nil response")
	}
	// The noopImagePullResponse should complete successfully.
	if waitErr := resp.Wait(context.Background()); waitErr != nil {
		t.Errorf("ImagePullResponse.Wait() = %v, want nil", waitErr)
	}
	_ = resp.Close()
}

// TestFakeRunClient_ImagePull_Error verifies that ImagePullErr is returned by
// ImagePull.
func TestFakeRunClient_ImagePull_Error(t *testing.T) {
	f := NewFakeRunClient(0)
	f.ImagePullErr = errors.New("registry unreachable")

	_, err := f.ImagePull(context.Background(), "alpine/socat:latest", moby.ImagePullOptions{})
	if err == nil {
		t.Fatal("expected error from ImagePull, got nil")
	}
	if !f.ImagePullCalled {
		t.Error("ImagePullCalled = false even when error occurs")
	}
}

// TestFakeRunClient_SocatImageMissing verifies the pull-on-demand simulation:
// ImageInspect for SocatImage returns not-found, while other images return found.
func TestFakeRunClient_SocatImageMissing(t *testing.T) {
	f := NewFakeRunClient(0)
	f.SocatImageMissing = true

	// Socat image: not found (triggers pull-on-demand in production).
	_, err := f.ImageInspect(context.Background(), SocatImage)
	if err == nil {
		t.Fatal("socat ImageInspect: expected not-found error, got nil")
	}
	if !cerrdefs.IsNotFound(err) {
		t.Fatalf("socat ImageInspect: expected cerrdefs.IsNotFound, got %v", err)
	}

	// Other image (e.g. the app image): still found — SocatImageMissing only
	// affects the socat image reference.
	_, err = f.ImageInspect(context.Background(), "claudebox:latest")
	if err != nil {
		t.Fatalf("app ImageInspect: expected image found, got %v", err)
	}

	// Repeated socat image check: still not-found (pull state is separate from
	// the fake — the fake models "socat not yet present").
	_, err = f.ImageInspect(context.Background(), SocatImage)
	if err == nil {
		t.Fatal("second socat ImageInspect: expected not-found error, got nil")
	}
}

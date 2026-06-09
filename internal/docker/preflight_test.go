package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
)

func TestCheckDaemonOK(t *testing.T) {
	f := newFakeRunClient(0) // PingErr unset → success
	d := newDockerWithClient(t, f)

	if err := d.CheckDaemon(context.Background()); err != nil {
		t.Fatalf("CheckDaemon() returned unexpected error: %v", err)
	}
}

func TestCheckDaemonDown(t *testing.T) {
	f := newFakeRunClient(0)
	f.PingErr = errors.New("connection refused")
	d := newDockerWithClient(t, f)

	err := d.CheckDaemon(context.Background())
	if err == nil {
		t.Fatal("CheckDaemon() expected error, got nil")
	}

	var dr *ErrDaemonUnreachable
	if !errors.As(err, &dr) {
		t.Fatalf("expected *ErrDaemonUnreachable, got %T: %v", err, err)
	}
	if dr.Cause == nil {
		t.Fatal("ErrDaemonUnreachable.Cause must not be nil")
	}
}

func TestImageExistsPresent(t *testing.T) {
	f := newFakeRunClient(0) // ImageMissing unset → found
	d := newDockerWithClient(t, f)

	found, err := d.ImageExists(context.Background(), "myimage:latest")
	if err != nil {
		t.Fatalf("ImageExists() unexpected error: %v", err)
	}
	if !found {
		t.Fatal("ImageExists() expected true, got false")
	}
}

// An absent image (cerrdefs.IsNotFound) yields (false, nil), not an error.
func TestImageExistsNotFound(t *testing.T) {
	f := newFakeRunClient(0)
	f.ImageMissing = true
	d := newDockerWithClient(t, f)

	found, err := d.ImageExists(context.Background(), "myimage:latest")
	if err != nil {
		t.Fatalf("ImageExists() expected nil error for missing image, got: %v", err)
	}
	if found {
		t.Fatal("ImageExists() expected false, got true")
	}
}

// Non-not-found errors must propagate, not be misclassified as "image absent" —
// a dead daemon must surface as a real error, not a misleading build hint.
func TestImageExistsOtherError(t *testing.T) {
	f := newFakeRunClient(0)
	f.ImageErr = errors.New("permission denied reading image store")
	d := newDockerWithClient(t, f)

	found, err := d.ImageExists(context.Background(), "myimage:latest")
	if err == nil {
		t.Fatal("ImageExists() expected error for other-error, got nil")
	}
	if found {
		t.Fatal("ImageExists() expected false when error occurs")
	}
	// Must not be classified as not-found (the predicate ImageExists itself uses).
	if cerrdefs.IsNotFound(err) {
		t.Fatal("other-error must not be treated as not-found")
	}
	if err.Error() != "permission denied reading image store" {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// CheckDaemon also works with fakeBuildClient (both fakes satisfy apiClient).
func TestCheckDaemonBuildClient(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f := newFakeBuildClient(0)
		d := newDockerWithClient(t, f)
		if err := d.CheckDaemon(context.Background()); err != nil {
			t.Fatalf("CheckDaemon() unexpected error: %v", err)
		}
	})
	t.Run("down", func(t *testing.T) {
		f := newFakeBuildClient(0)
		f.PingErr = errors.New("dial tcp: no such file or directory")
		d := newDockerWithClient(t, f)

		err := d.CheckDaemon(context.Background())
		if err == nil {
			t.Fatal("CheckDaemon() expected error, got nil")
		}
		var dr *ErrDaemonUnreachable
		if !errors.As(err, &dr) {
			t.Fatalf("expected *ErrDaemonUnreachable, got %T", err)
		}
	})
}

// A blocking Ping past the deadline must yield *ErrDaemonUnreachable wrapping
// context.DeadlineExceeded — proving the deadline is wired through. (Unit scope:
// does not prove a real SDK call against a black-hole DOCKER_HOST aborts.)
func TestCheckDaemonTimeoutBlockingPing(t *testing.T) {
	f := newFakeRunClient(0)
	f.BlockPing = true
	d := newDockerWithClient(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := d.CheckDaemon(ctx)
	if err == nil {
		t.Fatal("CheckDaemon() expected error when Ping blocks past deadline, got nil")
	}

	var dr *ErrDaemonUnreachable
	if !errors.As(err, &dr) {
		t.Fatalf("expected *ErrDaemonUnreachable, got %T: %v", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cause to be context.DeadlineExceeded, got: %v", dr.Cause)
	}
}

// A blocking ImageInspect past the deadline must return context.DeadlineExceeded,
// never misreported as "image absent" (only not-found yields (false, nil)).
func TestImageExistsTimeoutBlockingInspect(t *testing.T) {
	f := newFakeRunClient(0)
	f.BlockImageInspect = true
	d := newDockerWithClient(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	found, err := d.ImageExists(ctx, "myimage:latest")
	if err == nil {
		t.Fatal("ImageExists() expected error when ImageInspect blocks past deadline, got nil")
	}
	if found {
		t.Fatal("ImageExists() must return false on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if cerrdefs.IsNotFound(err) {
		t.Fatal("timeout error must not be misclassified as image-not-found")
	}
}

// Fast fake Ping/ImageInspect must succeed within the normal preflightTimeout.
func TestWithPreflightTimeoutHappyPath(t *testing.T) {
	f := newFakeRunClient(0) // instant success
	d := newDockerWithClient(t, f)

	ctx, cancel := WithPreflightTimeout(context.Background())
	defer cancel()

	if err := d.CheckDaemon(ctx); err != nil {
		t.Fatalf("CheckDaemon() unexpected error: %v", err)
	}

	ctx2, cancel2 := WithPreflightTimeout(context.Background())
	defer cancel2()

	found, err := d.ImageExists(ctx2, "myimage:latest")
	if err != nil {
		t.Fatalf("ImageExists() unexpected error: %v", err)
	}
	if !found {
		t.Fatal("ImageExists() expected true, got false")
	}
}

// Exercises the real WithPreflightTimeout helper (vs the hand-rolled deadline in
// TestCheckDaemonTimeoutBlockingPing): a non-deadline context would hang BlockPing.
func TestWithPreflightTimeout_BlockingPing(t *testing.T) {
	f := newFakeRunClient(0)
	f.BlockPing = true
	d := newDockerWithClient(t, f)

	ctx, cancel := WithPreflightTimeout(context.Background())
	defer cancel()
	cancel() // cancel immediately to avoid the 10 s real timeout

	err := d.CheckDaemon(ctx)
	if err == nil {
		t.Fatal("CheckDaemon() expected error on cancelled WithPreflightTimeout context, got nil")
	}
	var dr *ErrDaemonUnreachable
	if !errors.As(err, &dr) {
		t.Fatalf("expected *ErrDaemonUnreachable, got %T: %v", err, err)
	}
}

// WithPreflightTimeout must supply a deadline-carrying context to ImageExists.
func TestWithPreflightTimeout_BlockingInspect(t *testing.T) {
	f := newFakeRunClient(0)
	f.BlockImageInspect = true
	d := newDockerWithClient(t, f)

	ctx, cancel := WithPreflightTimeout(context.Background())
	defer cancel()
	cancel() // cancel immediately to avoid the 10 s real timeout

	found, err := d.ImageExists(ctx, "myimage:latest")
	if err == nil {
		t.Fatal("ImageExists() expected error on cancelled WithPreflightTimeout context, got nil")
	}
	if found {
		t.Fatal("ImageExists() must return false on cancelled context")
	}
	if cerrdefs.IsNotFound(err) {
		t.Fatal("cancellation error must not be misclassified as image-not-found")
	}
}

func TestErrDaemonUnreachableMessage(t *testing.T) {
	cause := errors.New("connection refused")

	t.Run("with endpoint", func(t *testing.T) {
		e := &ErrDaemonUnreachable{Endpoint: "unix:///var/run/docker.sock", Cause: cause}
		msg := e.Error()
		if msg == "" {
			t.Fatal("expected non-empty error message")
		}
		if !strings.Contains(msg, "unix:///var/run/docker.sock") {
			t.Errorf("endpoint not in message: %q", msg)
		}
		if !strings.Contains(msg, "connection refused") {
			t.Errorf("cause not in message: %q", msg)
		}
	})

	t.Run("without endpoint", func(t *testing.T) {
		e := &ErrDaemonUnreachable{Cause: cause}
		msg := e.Error()
		if msg == "" {
			t.Fatal("expected non-empty error message")
		}
		if !strings.Contains(msg, "connection refused") {
			t.Errorf("cause not in message: %q", msg)
		}
	})
}

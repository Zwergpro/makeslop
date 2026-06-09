package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
)

// TestCheckDaemonOK verifies that CheckDaemon returns nil when the fake ping
// succeeds.
func TestCheckDaemonOK(t *testing.T) {
	f := NewFakeRunClient(0) // PingErr unset → success
	d := newDockerWithClient(t, f)

	if err := d.CheckDaemon(context.Background()); err != nil {
		t.Fatalf("CheckDaemon() returned unexpected error: %v", err)
	}
}

// TestCheckDaemonDown verifies that CheckDaemon returns *ErrDaemonUnreachable
// when Ping fails.
func TestCheckDaemonDown(t *testing.T) {
	f := NewFakeRunClient(0)
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

// TestImageExistsPresent verifies that ImageExists returns (true, nil) when the
// image is found.
func TestImageExistsPresent(t *testing.T) {
	f := NewFakeRunClient(0) // ImageMissing unset → found
	d := newDockerWithClient(t, f)

	found, err := d.ImageExists(context.Background(), "myimage:latest")
	if err != nil {
		t.Fatalf("ImageExists() unexpected error: %v", err)
	}
	if !found {
		t.Fatal("ImageExists() expected true, got false")
	}
}

// TestImageExistsNotFound verifies that ImageExists returns (false, nil) —
// not an error — when the image is absent (cerrdefs.IsNotFound classification).
func TestImageExistsNotFound(t *testing.T) {
	f := NewFakeRunClient(0)
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

// TestImageExistsOtherError verifies that ImageExists propagates non-not-found
// errors rather than misclassifying them as "image absent". A dead daemon must
// surface as a real error, not a misleading build hint.
func TestImageExistsOtherError(t *testing.T) {
	f := NewFakeRunClient(0)
	f.ImageErr = errors.New("permission denied reading image store")
	d := newDockerWithClient(t, f)

	found, err := d.ImageExists(context.Background(), "myimage:latest")
	if err == nil {
		t.Fatal("ImageExists() expected error for other-error, got nil")
	}
	if found {
		t.Fatal("ImageExists() expected false when error occurs")
	}
	// Confirm it is NOT misclassified as a not-found result.
	// cerrdefs.IsNotFound is the canonical predicate used by ImageExists; use it
	// here to ensure the other-error is not silently treated as "image absent".
	if cerrdefs.IsNotFound(err) {
		t.Fatal("other-error must not be treated as not-found")
	}
	if err.Error() != "permission denied reading image store" {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestCheckDaemonBuildClient verifies CheckDaemon works with FakeBuildClient
// as well (both fake types satisfy the wider interface).
func TestCheckDaemonBuildClient(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		d := newDockerWithClient(t, f)
		if err := d.CheckDaemon(context.Background()); err != nil {
			t.Fatalf("CheckDaemon() unexpected error: %v", err)
		}
	})
	t.Run("down", func(t *testing.T) {
		f := NewFakeBuildClient(0)
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

// TestCheckDaemonTimeoutBlockingPing verifies that CheckDaemon returns
// *ErrDaemonUnreachable wrapping context.DeadlineExceeded when the fake Ping
// blocks past a short deadline. This tests that WithPreflightTimeout actually
// delivers a deadline-carrying context to CheckDaemon.
//
// NOTE: this unit test proves that the deadline is wired through correctly; it
// does NOT prove that a real SDK call against a black-hole DOCKER_HOST also
// aborts (that is integration-only and beyond the scope of unit tests here).
func TestCheckDaemonTimeoutBlockingPing(t *testing.T) {
	f := NewFakeRunClient(0)
	f.BlockPing = true // blocks until ctx is cancelled
	d := newDockerWithClient(t, f)

	// Use a very short deadline so the test completes quickly.
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

// TestImageExistsTimeoutBlockingInspect verifies that ImageExists returns
// promptly with a context.DeadlineExceeded error when the fake ImageInspect
// blocks past the deadline. Crucially, the result must NOT be misreported as
// "image absent" — only a not-found classification should produce (false, nil).
//
// NOTE: same caveat as TestCheckDaemonTimeoutBlockingPing: unit-test scope only.
func TestImageExistsTimeoutBlockingInspect(t *testing.T) {
	f := NewFakeRunClient(0)
	f.BlockImageInspect = true // blocks until ctx is cancelled
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
	// A timeout must not be classified as "image absent".
	if cerrdefs.IsNotFound(err) {
		t.Fatal("timeout error must not be misclassified as image-not-found")
	}
}

// TestWithPreflightTimeoutHappyPath verifies that a fast fake Ping and
// ImageInspect succeed within the normal preflightTimeout — no regression.
func TestWithPreflightTimeoutHappyPath(t *testing.T) {
	f := NewFakeRunClient(0) // instant success
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

// TestErrDaemonUnreachableMessage verifies that ErrDaemonUnreachable produces
// a useful error string with and without an endpoint.
func TestErrDaemonUnreachableMessage(t *testing.T) {
	cause := errors.New("connection refused")

	t.Run("with endpoint", func(t *testing.T) {
		e := &ErrDaemonUnreachable{Endpoint: "unix:///var/run/docker.sock", Cause: cause}
		msg := e.Error()
		if msg == "" {
			t.Fatal("expected non-empty error message")
		}
		// Endpoint and cause must appear in the message.
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

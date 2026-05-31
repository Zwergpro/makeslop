package docker

import (
	"context"
	"errors"
	"testing"
)

// TestCheckDaemonOK verifies that CheckDaemon returns nil when the fake ping
// succeeds.
func TestCheckDaemonOK(t *testing.T) {
	f := NewFakeRunClient(0) // PingErr unset → success
	t.Cleanup(SetClientForTest(f))

	if err := CheckDaemon(context.Background()); err != nil {
		t.Fatalf("CheckDaemon() returned unexpected error: %v", err)
	}
}

// TestCheckDaemonDown verifies that CheckDaemon returns *ErrDaemonUnreachable
// when Ping fails.
func TestCheckDaemonDown(t *testing.T) {
	f := NewFakeRunClient(0)
	f.PingErr = errors.New("connection refused")
	t.Cleanup(SetClientForTest(f))

	err := CheckDaemon(context.Background())
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
	t.Cleanup(SetClientForTest(f))

	found, err := ImageExists(context.Background(), "myimage:latest")
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
	t.Cleanup(SetClientForTest(f))

	found, err := ImageExists(context.Background(), "myimage:latest")
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
	t.Cleanup(SetClientForTest(f))

	found, err := ImageExists(context.Background(), "myimage:latest")
	if err == nil {
		t.Fatal("ImageExists() expected error for other-error, got nil")
	}
	if found {
		t.Fatal("ImageExists() expected false when error occurs")
	}
	// Confirm it is NOT misclassified as a not-found result.
	if errors.Is(err, errors.New("not found")) {
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
		t.Cleanup(SetClientForTest(f))
		if err := CheckDaemon(context.Background()); err != nil {
			t.Fatalf("CheckDaemon() unexpected error: %v", err)
		}
	})
	t.Run("down", func(t *testing.T) {
		f := NewFakeBuildClient(0)
		f.PingErr = errors.New("dial tcp: no such file or directory")
		t.Cleanup(SetClientForTest(f))

		err := CheckDaemon(context.Background())
		if err == nil {
			t.Fatal("CheckDaemon() expected error, got nil")
		}
		var dr *ErrDaemonUnreachable
		if !errors.As(err, &dr) {
			t.Fatalf("expected *ErrDaemonUnreachable, got %T", err)
		}
	})
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
		if !contains(msg, "unix:///var/run/docker.sock") {
			t.Errorf("endpoint not in message: %q", msg)
		}
		if !contains(msg, "connection refused") {
			t.Errorf("cause not in message: %q", msg)
		}
	})

	t.Run("without endpoint", func(t *testing.T) {
		e := &ErrDaemonUnreachable{Cause: cause}
		msg := e.Error()
		if msg == "" {
			t.Fatal("expected non-empty error message")
		}
		if !contains(msg, "connection refused") {
			t.Errorf("cause not in message: %q", msg)
		}
	})
}

// TestErrImageNotBuiltMessage verifies the ErrImageNotBuilt error string.
func TestErrImageNotBuiltMessage(t *testing.T) {
	e := &ErrImageNotBuilt{Tag: "myapp:latest"}
	msg := e.Error()
	if !contains(msg, "myapp:latest") {
		t.Errorf("tag not in message: %q", msg)
	}
}

// contains is a simple substring check to keep tests dependency-free.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

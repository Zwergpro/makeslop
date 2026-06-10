package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/assets"
	"github.com/Zwergpro/makeslop/internal/config"
)

// newFakeBuildDocker builds a fakeDocker whose Build fails for a non-zero exitCode.
func newFakeBuildDocker(exitCode int) *fakeDocker {
	fd := &fakeDocker{}
	if exitCode != 0 {
		fd.BuildErr = fmt.Errorf("build exited with code %d", exitCode)
	}
	return fd
}

// build on a fresh baseDir self-heals the Dockerfile then invokes Build.
func TestBuild_SeedsSelfHealAndInvokesSDK(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build")
	if err != nil {
		t.Fatalf("build failed: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}

	dockerfilePath := filepath.Join(baseDir, config.DockerfileFile)
	if _, statErr := os.Stat(dockerfilePath); statErr != nil {
		t.Errorf("Dockerfile not seeded by build: %v", statErr)
	}

	if fbc.LastBuildOpts.Image != "claudebox" {
		t.Errorf("Build Image = %q, want %q", fbc.LastBuildOpts.Image, "claudebox")
	}

	if filepath.Base(fbc.LastBuildOpts.DockerfilePath) != "Dockerfile" {
		t.Errorf("Build DockerfilePath basename = %q, want %q", filepath.Base(fbc.LastBuildOpts.DockerfilePath), "Dockerfile")
	}
	// BuildKit version selection is covered in internal/docker build_test.go.
}

func TestBuild_NoCacheAndBuildArg(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build", "--no-cache", "--build-arg", "GO_VERSION=1.26.3")
	if err != nil {
		t.Fatalf("build --no-cache --build-arg failed: %v; stderr=%q", err, stderr)
	}

	if !fbc.LastBuildOpts.NoCache {
		t.Error("BuildOptions.NoCache must be true when --no-cache is passed")
	}
	var foundGOVersion bool
	for _, arg := range fbc.LastBuildOpts.BuildArgs {
		if arg == "GO_VERSION=1.26.3" {
			foundGOVersion = true
			break
		}
	}
	if !foundGOVersion {
		t.Errorf("BuildOptions.BuildArgs missing GO_VERSION=1.26.3; got %v", fbc.LastBuildOpts.BuildArgs)
	}
}

func TestBuild_NonZeroExit_PropagatesCode(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(1)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fbc), []string{"build"})
	if code != 1 {
		t.Errorf("runWithExitCode = %d, want 1 (generic error); stderr=%q", code, stderr.String())
	}
}

// A custom image name in settings.json flows into the Build options.
func TestBuild_CustomImage_FromSettings(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	if err := config.Bootstrap(baseDir); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	s.Image = "my-custom-image:v2"
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build")
	if err != nil {
		t.Fatalf("build failed: %v; stderr=%q", err, stderr)
	}

	if fbc.LastBuildOpts.Image != "my-custom-image:v2" {
		t.Errorf("Build Image = %q, want %q", fbc.LastBuildOpts.Image, "my-custom-image:v2")
	}
}

func TestBuild_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "build")
	if err == nil {
		t.Fatalf("expected error from build with corrupt settings, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("expected error to mention 'settings' context, got %q", err.Error())
	}
}

// --build-arg is repeatable; all values forwarded.
func TestBuild_MultipleBuildArgs(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build",
		"--build-arg", "GO_VERSION=1.26.3",
		"--build-arg", "HTTP_PROXY=http://proxy.example.com:8080",
		"--build-arg", "FOO=bar",
	)
	if err != nil {
		t.Fatalf("build --build-arg (multiple) failed: %v; stderr=%q", err, stderr)
	}

	wantArgs := []string{"GO_VERSION=1.26.3", "HTTP_PROXY=http://proxy.example.com:8080", "FOO=bar"}
	for _, want := range wantArgs {
		var found bool
		for _, arg := range fbc.LastBuildOpts.BuildArgs {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("BuildOptions.BuildArgs missing %q; got %v", want, fbc.LastBuildOpts.BuildArgs)
		}
	}
}

// build --refresh overwrites a stale Dockerfile with the embedded assets version.
func TestBuild_Refresh_OverwritesDockerfileAndBuilds(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	// Bootstrap is no-overwrite, so the STALE marker survives without --refresh.
	if err := config.Bootstrap(baseDir); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	staleContent := []byte("# STALE\nFROM scratch\n")
	dockerfilePath := filepath.Join(baseDir, config.DockerfileFile)
	if err := os.WriteFile(dockerfilePath, staleContent, 0o644); err != nil {
		t.Fatalf("write stale Dockerfile: %v", err)
	}

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build", "--refresh")
	if err != nil {
		t.Fatalf("build --refresh failed: %v; stderr=%q", err, stderr)
	}

	got, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile after --refresh: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile after --refresh does not match embedded assets:\ngot  (%d bytes)\nwant (%d bytes)",
			len(got), len(assets.Dockerfile))
	}

	if fbc.LastBuildOpts.Image == "" {
		t.Error("Build was not called after --refresh")
	}
}

// Plain build (no --refresh) must not overwrite a hand-edited Dockerfile.
func TestBuild_NoRefresh_LeavesDockerfileIntact(t *testing.T) {
	baseDir := t.TempDir()
	fbc := newFakeBuildDocker(0)

	if err := config.Bootstrap(baseDir); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	staleContent := []byte("# STALE\nFROM scratch\n")
	dockerfilePath := filepath.Join(baseDir, config.DockerfileFile)
	if err := os.WriteFile(dockerfilePath, staleContent, 0o644); err != nil {
		t.Fatalf("write stale Dockerfile: %v", err)
	}

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build")
	if err != nil {
		t.Fatalf("build (no --refresh) failed: %v; stderr=%q", err, stderr)
	}

	got, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile after plain build: %v", err)
	}
	if !bytes.Equal(got, staleContent) {
		t.Errorf("plain build must not overwrite Dockerfile:\ngot  %q\nwant %q", got, staleContent)
	}
}

// --quiet suppresses the "refreshed…" stderr notice; non-quiet emits it.
func TestBuild_Refresh_Quiet_SuppressesNotice(t *testing.T) {
	t.Run("quiet suppresses notice", func(t *testing.T) {
		baseDir := t.TempDir()
		fbc := newFakeBuildDocker(0)

		_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build", "--refresh", "--quiet")
		if err != nil {
			t.Fatalf("build --refresh --quiet failed: %v; stderr=%q", err, stderr)
		}
		if strings.Contains(stderr, "refreshed") {
			t.Errorf("--quiet must suppress the 'refreshed' notice; stderr=%q", stderr)
		}
	})

	t.Run("non-quiet emits notice", func(t *testing.T) {
		baseDir := t.TempDir()
		fbc := newFakeBuildDocker(0)

		_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build", "--refresh")
		if err != nil {
			t.Fatalf("build --refresh failed: %v; stderr=%q", err, stderr)
		}
		if !strings.Contains(stderr, "refreshed") {
			t.Errorf("without --quiet the 'refreshed' notice must appear; stderr=%q", stderr)
		}
		if !strings.Contains(stderr, "~/.makeslop/Dockerfile") {
			t.Errorf("notice must mention ~/.makeslop/Dockerfile; stderr=%q", stderr)
		}
	})
}

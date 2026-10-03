package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// hasMountWithContainer returns true iff spec.Mounts contains a mount
// whose Container field equals target.
func hasMountWithContainer(mounts []docker.Mount, target string) bool {
	_, ok := findMount(mounts, target)
	return ok
}

// findMount returns the first mount whose Container field equals container.
func findMount(mounts []docker.Mount, container string) (docker.Mount, bool) {
	for _, m := range mounts {
		if m.Container == container {
			return m, true
		}
	}
	return docker.Mount{}, false
}

// hasMountWithContainerAndHost returns true iff spec.Mounts contains a mount
// whose Container field equals containerTarget and Host field equals hostTarget.
func hasMountWithContainerAndHost(mounts []docker.Mount, containerTarget, hostTarget string) bool {
	for _, m := range mounts {
		if m.Container == containerTarget && m.Host == hostTarget {
			return true
		}
	}
	return false
}

func TestRun_NotRegistered_NoMutation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	beforeFiles := listFiles(t, baseDir)
	if len(beforeFiles) != 0 {
		t.Fatalf("baseDir not empty before run: %v", beforeFiles)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "-i", "test-img")
	if err == nil {
		t.Fatalf("expected error from makeslop go, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing 'no workspace registered': %q", stderr)
	}
	if !strings.Contains(stderr, "— run 'makeslop init'") {
		t.Errorf("stderr missing remedy '— run 'makeslop init'': %q", stderr)
	}
	resolvedPwd := evalSymlinks(t, pwd)
	if !strings.Contains(stderr, resolvedPwd) {
		t.Errorf("stderr missing pwd %q: %q", resolvedPwd, stderr)
	}

	afterFiles := listFiles(t, baseDir)
	if len(afterFiles) != 0 {
		t.Fatalf("baseDir should be untouched, found: %v", afterFiles)
	}
}

func TestRun_AfterInit_LaunchesDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	fc := newFakeDocker(0, true)

	snapBefore := snapshotTree(t, baseDir)
	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("root failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("makeslop go must not print on stdout (milestone-1 path was removed); got %q", stdout)
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)

	if !fc.Started {
		t.Error("docker.Run must have been invoked (fc.Started must be true)")
	}

	resolvedPwd := evalSymlinks(t, pwd)
	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}

	// Verify fc.LastSpec carries the correct wiring from runRun → BuildSpec.
	// Field-level assertions catch bugs where runRun passes the wrong value
	// (e.g. wrong image, wrong project root) even when fc.Started is true.
	got := fc.LastSpec
	if got.Image != s.Image {
		t.Errorf("fc.LastSpec.Image = %q, want %q", got.Image, s.Image)
	}
	if got.Command != s.Shell {
		t.Errorf("fc.LastSpec.Command = %q, want %q", got.Command, s.Shell)
	}
	// The workspace mount must bind the registered project root (resolvedPwd).
	wantMountSource := resolvedPwd
	wantMountTarget := "/workspace/" + filepath.Base(workspaceDir)
	if !hasMountWithContainerAndHost(got.Mounts, wantMountTarget, wantMountSource) {
		t.Errorf("fc.LastSpec missing workspace mount source=%q target=%q; mounts=%v",
			wantMountSource, wantMountTarget, got.Mounts)
	}
}

func TestRun_FromSubdirectory_MountsRegisteredAncestor(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	parent := t.TempDir()
	t.Chdir(parent)
	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	sub := filepath.Join(parent, "deeply", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(sub)

	fc := newFakeDocker(0, true)

	if _, _, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); err != nil {
		t.Fatalf("root failed: %v", err)
	}

	if !fc.Started {
		t.Error("docker.Run must have been invoked for a registered workspace")
	}

	// fc.LastSpec must mount the registered ancestor (parent), not the subdir.
	// Inspecting fc.LastSpec directly catches wiring bugs in runRun that would
	// not be visible from a locally-reconstructed spec.
	resolvedParent := evalSymlinks(t, parent)
	wantMountSource := resolvedParent
	wantMountTarget := "/workspace/" + filepath.Base(workspaceDir)
	var foundWorkspaceMount bool
	for _, m := range fc.LastSpec.Mounts {
		if m.Host == wantMountSource && m.Container == wantMountTarget {
			foundWorkspaceMount = true
			break
		}
	}
	if !foundWorkspaceMount {
		t.Errorf("fc.LastSpec missing workspace mount source=%q target=%q; mounts=%v",
			wantMountSource, wantMountTarget, fc.LastSpec.Mounts)
	}
}

func TestRun_Unregistered_DoesNotInvokeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "-i", "test-img")
	if err == nil {
		t.Fatalf("expected error from unregistered makeslop go, got nil")
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker client must not be started for unregistered workspace")
	}
}

// Exercises the production ttyCheck (pipes in `go test` are not TTYs).
func TestRun_NoTTY_FailsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	fc := newFakeDocker(0, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when stdin/stdout are not TTYs, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (cobra layer wrote tailored message), got %v", err)
	}
	if !strings.Contains(stderr, "TTY") {
		t.Errorf("stderr missing TTY hint: %q", stderr)
	}
	if !strings.Contains(stderr, "— run in an interactive terminal") {
		t.Errorf("stderr missing remedy '— run in an interactive terminal': %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker client must not be started when TTY check fails")
	}
}

func TestRun_ExitCodePropagation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	fc := newFakeDocker(42, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code != 42 {
		t.Errorf("runWithExitCode = %d, want 42; stderr=%q", code, stderr.String())
	}
}

// Guards that settings.json values reach the docker invocation, not just compiled-in defaults.
func TestRun_CustomImageAndShell_FlowFromSettings(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	s.Image = "my-img:tag"
	s.Shell = "/bin/dash"
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stdout, "my-img:tag") {
		t.Errorf("--dry-run output missing custom image 'my-img:tag'; stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "/bin/dash") {
		t.Errorf("--dry-run output missing custom shell '/bin/dash'; stdout=%q", stdout)
	}
}

// Guards that non-ErrNotRegistered errors from Lookup surface through cobra's SilenceErrors.
func TestRun_CorruptSettings_ReportsError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, _, err := runCmd(t, baseDir, "run")
	if err == nil {
		t.Fatalf("expected error from makeslop go with corrupt settings, got nil; stdout=%q", stdout)
	}
	if errors.Is(err, errSilent) {
		t.Errorf("corrupt-settings error must not be errSilent — main() needs to print it: %v", err)
	}
	if errors.Is(err, workspace.ErrNotRegistered) {
		t.Errorf("corrupt-settings error must not be ErrNotRegistered: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("expected error to mention 'settings' context, got %q", err.Error())
	}
}

func TestRun_NotRegistered_ReturnsErrSilent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "run", "-i", "test-img")
	if err == nil {
		t.Fatalf("expected error from makeslop go, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (so main() skips reprint), got %v", err)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
}

// Guards that docker is never invoked when cwd is outside HOME.
func TestRun_OutsideHome_Refuses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	fc := newFakeDocker(0, true)

	snapBefore := snapshotTree(t, baseDir)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error from makeslop go outside HOME, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "refusing to run from") {
		t.Errorf("stderr missing 'refusing to run from': %q", stderr)
	}
	if !strings.Contains(stderr, "— pass --out-of-home to override") {
		t.Errorf("stderr missing remedy '— pass --out-of-home to override': %q", stderr)
	}
	if !strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr does not end with newline: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker client must not be started when outside HOME")
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

func TestOutOfHomeFlag_Bypasses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	_, stderr, err := runCmd(t, baseDir, "init", "--out-of-home")
	if err != nil {
		t.Fatalf("init --out-of-home should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("init --out-of-home: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}
	if _, stderr, err := runCmd(t, baseDir, "config", "set", "image", "test-img"); err != nil {
		t.Fatalf("config set image failed: %v; stderr=%q", err, stderr)
	}

	fc := newFakeDocker(0, true)

	_, stderr, err = runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "--out-of-home")
	if err != nil {
		t.Fatalf("makeslop run --out-of-home should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("makeslop --out-of-home go: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}
	if !fc.Started {
		t.Errorf("docker client was not started when --out-of-home bypasses guard")
	}
}

func TestRun_MasksFoundEnvFiles_ArgvContainsDevNullMounts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	resolvedPwd := evalSymlinks(t, pwd)

	envFile1 := filepath.Join(resolvedPwd, ".env")
	envFile2 := filepath.Join(resolvedPwd, "configs", "local.env")
	if err := os.MkdirAll(filepath.Dir(envFile2), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	if err := os.WriteFile(envFile1, []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(envFile2, []byte("SECRET=2"), 0o644); err != nil {
		t.Fatalf("write local.env: %v", err)
	}

	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n      - \".env.*\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "makeslop: masked 2 secret file(s)") {
		t.Errorf("stderr missing masked count: %q", stderr)
	}

	name := filepath.Base(workspaceDir)
	wantMount1 := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	wantMount2 := "type=bind,source=/dev/null,target=/workspace/" + name + "/configs/local.env"
	if !strings.Contains(stdout, wantMount1) {
		t.Errorf("--dry-run stdout missing /dev/null mount for .env: want %q\nstdout:\n%s", wantMount1, stdout)
	}
	if !strings.Contains(stdout, wantMount2) {
		t.Errorf("--dry-run stdout missing /dev/null mount for local.env: want %q\nstdout:\n%s", wantMount2, stdout)
	}

	// Overlay mounts must come after the project bind mount (tail ordering).
	projectMount := "source=" + resolvedPwd + ",target=/workspace/" + name
	projectIdx := strings.Index(stdout, projectMount)
	env1Idx := strings.Index(stdout, wantMount1)
	env2Idx := strings.Index(stdout, wantMount2)
	if projectIdx < 0 {
		t.Errorf("stdout missing project bind mount %q", projectMount)
	}
	if env1Idx >= 0 && env1Idx <= projectIdx {
		t.Errorf("/dev/null mount for .env (byte %d) must appear after project bind mount (byte %d)", env1Idx, projectIdx)
	}
	if env2Idx >= 0 && env2Idx <= projectIdx {
		t.Errorf("/dev/null mount for local.env (byte %d) must appear after project bind mount (byte %d)", env2Idx, projectIdx)
	}
}

func TestRun_NoEnvFiles_PrintsNothingExtraOnStderr(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "masked") {
		t.Errorf("stderr must not mention 'masked' when no .env files found: %q", stderr)
	}
	for _, a := range strings.Fields(stdout) {
		if strings.Contains(a, "/dev/null") {
			t.Errorf("output must not contain /dev/null mounts when no .env files found: %q", a)
		}
	}
}

// ── Helper unit tests (mergeUniqueSorted) ─────────────────────────────────────

func TestMergeUniqueSorted(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: nil,
		},
		{
			name: "both empty",
			a:    []string{},
			b:    []string{},
			want: nil,
		},
		{
			name: "a only",
			a:    []string{"c", "a", "b"},
			b:    nil,
			want: []string{"a", "b", "c"},
		},
		{
			name: "b only",
			b:    []string{"z", "x", "y"},
			want: []string{"x", "y", "z"},
		},
		{
			name: "no overlap",
			a:    []string{"a", "b"},
			b:    []string{"c", "d"},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "within-list duplicates in a",
			a:    []string{"a", "a", "b"},
			b:    []string{"c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "within-list duplicates in b",
			a:    []string{"a"},
			b:    []string{"b", "b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "cross-list duplicates",
			a:    []string{"a", "b"},
			b:    []string{"b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "all duplicates",
			a:    []string{"x", "y"},
			b:    []string{"x", "y"},
			want: []string{"x", "y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeUniqueSorted(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("mergeUniqueSorted(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("mergeUniqueSorted result[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── --dry-run tests ────────────────────────────────────────────────────────────

// --dry-run succeeds even when TTY is false (no docker exec).
func TestRun_DryRun_SkipsDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, false)

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run should succeed, got err: %v; stderr=%q", err, stderr)
	}
	if fc.Started {
		t.Errorf("docker client must not be started on --dry-run")
	}
	if stdout == "" {
		t.Errorf("--dry-run must print to stdout; got empty")
	}
}

// --dry-run stdout must equal BuildSpec(opts).ShellCommand() (single source of truth).
func TestRun_DryRun_StdoutEqualsBuildSpecShellCommand(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	resolvedPwd := evalSymlinks(t, pwd)
	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	// init scaffolds .makeslop.yaml ⇒ ProtectProjectConfig true; both cache groups default to true.
	// t.TempDir() has no .git directory — MaskGitHooks stays false.
	want := docker.BuildSpec(docker.Options{
		Projects: []docker.Project{{
			Host:          resolvedPwd,
			Name:          filepath.Base(workspaceDir),
			Label:         "project: " + resolvedPwd,
			ProtectConfig: true,
		}},
		WorkspaceHost:     workspaceDir,
		BaseDir:           baseDir,
		Image:             s.Image,
		Command:           s.Shell,
		TmpDirSize:        s.TmpDirSize,
		MountContentCache: true,
		MountAgentCache:   true,
	}).ShellCommand()

	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("stdout mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRun_DryRun_ShortFlag(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	stdoutLong, stderrLong, errLong := runCmd(t, baseDir, "run", "--dry-run")
	if errLong != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", errLong, stderrLong)
	}

	stdoutShort, stderrShort, errShort := runCmd(t, baseDir, "run", "-n")
	if errShort != nil {
		t.Fatalf("-n failed: %v; stderr=%q", errShort, stderrShort)
	}

	if stdoutShort != stdoutLong {
		t.Errorf("-n stdout != --dry-run stdout\nshort:\n%s\nlong:\n%s", stdoutShort, stdoutLong)
	}
}

// TTY-bypass guard: --dry-run succeeds even when real ttyCheck returns false
// because docker.Run (the only ttyCheck caller) is never invoked.
func TestRun_DryRun_NoTTY_Succeeds(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	// Real ttyCheck returns false under go test; docker.Run must never be reached.
	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run must succeed with no TTY (TTY check lives in docker.Run which is skipped); err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("--dry-run must print command to stdout; got empty")
	}
}

func TestRun_DryRun_Unregistered_StillRefuses(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd) // no init — workspace not registered

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run", "-i", "test-img")
	if err == nil {
		t.Fatalf("expected error for unregistered workspace, got nil; stdout=%q", stdout)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing 'no workspace registered': %q", stderr)
	}
	if !strings.Contains(stderr, "— run 'makeslop init'") {
		t.Errorf("stderr missing remedy '— run 'makeslop init'': %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when precondition fails; got %q", stdout)
	}
}

func TestRun_DryRun_OutsideHome_StillRefuses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err == nil {
		t.Fatalf("expected error from --dry-run outside HOME, got nil; stdout=%q", stdout)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "refusing to run from") {
		t.Errorf("stderr missing 'refusing to run from': %q", stderr)
	}
	if !strings.Contains(stderr, "— pass --out-of-home to override") {
		t.Errorf("stderr missing remedy '— pass --out-of-home to override': %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when home-dir guard fires; got %q", stdout)
	}
}

func TestRun_DryRun_OutOfHomeBypasses(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()

	insidePwd := t.TempDir()
	t.Chdir(insidePwd)
	initWithImage(t, baseDir)

	newHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, newHome))

	t.Chdir(insidePwd)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--out-of-home", "--dry-run")
	if err != nil {
		t.Fatalf("--out-of-home --dry-run should succeed; err=%v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("stderr must not contain 'refusing to run' when --out-of-home is set: %q", stderr)
	}
	if stdout == "" {
		t.Errorf("--out-of-home --dry-run must print command to stdout")
	}
}

// Guards that precondition errors (ws.Lookup → config.Load) propagate under --dry-run.
func TestRun_DryRun_CorruptSettings(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Register, then corrupt settings so ws.Lookup fails.
	initWithImage(t, baseDir)
	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt settings: %v", err)
	}

	stdout, _, err := runCmd(t, baseDir, "run", "--dry-run")
	if err == nil {
		t.Fatalf("expected error for corrupt settings under --dry-run, got nil; stdout=%q", stdout)
	}
	// Must NOT be errSilent — main() must print the wrapped error.
	if errors.Is(err, errSilent) {
		t.Errorf("corrupt-settings error must not be errSilent: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when config.Load fails; got %q", stdout)
	}
}

func TestRun_DryRun_MasksEnvFiles_StdoutContainsDevNullMounts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	resolvedPwd := evalSymlinks(t, pwd)

	envFile1 := filepath.Join(resolvedPwd, ".env")
	envFile2 := filepath.Join(resolvedPwd, "configs", "local.env")
	if err := os.MkdirAll(filepath.Dir(envFile2), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	if err := os.WriteFile(envFile1, []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(envFile2, []byte("SECRET=2"), 0o644); err != nil {
		t.Fatalf("write local.env: %v", err)
	}

	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n      - \".env.*\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "makeslop: masked 2 secret file(s)") {
		t.Errorf("stderr missing masked count: %q", stderr)
	}

	name := filepath.Base(workspaceDir)
	wantMount1 := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	wantMount2 := "type=bind,source=/dev/null,target=/workspace/" + name + "/configs/local.env"
	if !strings.Contains(stdout, wantMount1) {
		t.Errorf("stdout missing /dev/null mount for .env: want substring %q\nstdout:\n%s", wantMount1, stdout)
	}
	if !strings.Contains(stdout, wantMount2) {
		t.Errorf("stdout missing /dev/null mount for local.env: want substring %q\nstdout:\n%s", wantMount2, stdout)
	}

	// Overlay mounts must come after the project bind mount (mount-order invariant).
	projectMount := "source=" + resolvedPwd + ",target=/workspace/" + name
	projectIdx := strings.Index(stdout, projectMount)
	env1Idx := strings.Index(stdout, wantMount1)
	env2Idx := strings.Index(stdout, wantMount2)
	if projectIdx < 0 {
		t.Errorf("stdout missing project bind mount %q\nstdout:\n%s", projectMount, stdout)
	}
	if env1Idx >= 0 && env1Idx <= projectIdx {
		t.Errorf("/dev/null mount for .env (byte %d) must appear after project bind mount (byte %d)", env1Idx, projectIdx)
	}
	if env2Idx >= 0 && env2Idx <= projectIdx {
		t.Errorf("/dev/null mount for local.env (byte %d) must appear after project bind mount (byte %d)", env2Idx, projectIdx)
	}
}

func TestRun_DryRun_FromSubdir_MountsAncestor(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	parent := t.TempDir()
	t.Chdir(parent)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	sub := filepath.Join(parent, "deeply", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(sub)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run from subdir failed: %v; stderr=%q", err, stderr)
	}

	resolvedParent := evalSymlinks(t, parent)
	wantFragment := "source=" + resolvedParent + ",target=/workspace/" + filepath.Base(workspaceDir)
	if !strings.Contains(stdout, wantFragment) {
		t.Errorf("stdout missing project-root mount fragment %q\nstdout:\n%s", wantFragment, stdout)
	}
}

// ── projectconfig.Load wiring tests ───────────────────────────────────────────

// Opt-in masking: absent scan patterns ⇒ nothing masked even when secrets exist on disk.
func TestRun_EmptyScanPatterns_NoFilesMasked(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	envFile := filepath.Join(resolvedPwd, ".env")
	if err := os.WriteFile(envFile, []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	yamlContent := "exclude:\n  scan:\n    patterns: []\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("go must succeed when exclude.scan.patterns is empty; err=%v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "masked") {
		t.Errorf("stderr must not mention 'masked' when exclude.scan.patterns is empty: %q", stderr)
	}
}

func TestRun_LoadsYamlAndMergesMaskedFiles(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	envFile := filepath.Join(resolvedPwd, ".env")
	secretFile := filepath.Join(resolvedPwd, "private", "token.txt")
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o755); err != nil {
		t.Fatalf("mkdir private: %v", err)
	}
	if err := os.WriteFile(envFile, []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(secretFile, []byte("tok"), 0o644); err != nil {
		t.Fatalf("write token.txt: %v", err)
	}

	// Scan finds .env; private/token.txt comes via exclude.files (not counted in masked N).
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  files: [private/token.txt]\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stderr, "masked 1 secret file") {
		t.Errorf("stderr must mention 'masked 1 secret file'; got %q", stderr)
	}

	name := filepath.Base(workspaceDir)
	wantMount1 := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	wantMount2 := "type=bind,source=/dev/null,target=/workspace/" + name + "/private/token.txt"
	if !strings.Contains(stdout, wantMount1) {
		t.Errorf("--dry-run stdout missing /dev/null mount for .env: want %q\nstdout:\n%s", wantMount1, stdout)
	}
	if !strings.Contains(stdout, wantMount2) {
		t.Errorf("--dry-run stdout missing /dev/null mount for private/token.txt: want %q\nstdout:\n%s", wantMount2, stdout)
	}

	// Lex order: .env < private/token.txt.
	idx1 := strings.Index(stdout, wantMount1)
	idx2 := strings.Index(stdout, wantMount2)
	if idx1 >= 0 && idx2 >= 0 && idx1 >= idx2 {
		t.Errorf("/dev/null mount for .env (byte %d) should come before private/token.txt (byte %d) in lex order", idx1, idx2)
	}
}

func TestRun_BadScanPattern_AbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Invalid glob (unclosed bracket).
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"[bad\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, _, err := runCmd(t, baseDir, "run")
	if err == nil {
		t.Fatal("makeslop go must fail with a bad scan pattern, got nil error")
	}
}

func TestRun_LoadsYamlMaskedDirs_TmpfsMountInArgv(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	// The directory must exist on disk so projectconfig.Load keeps it.
	nodeModules := filepath.Join(resolvedPwd, "node_modules")
	if err := os.MkdirAll(nodeModules, 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}

	yamlContent := "exclude:\n  dirs: [node_modules]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	wantTmpfs := "type=tmpfs,target=/workspace/" + name + "/node_modules"
	projectMount := "source=" + resolvedPwd + ",target=/workspace/" + name

	if !strings.Contains(stdout, wantTmpfs) {
		t.Errorf("--dry-run stdout missing tmpfs mount: want %q\nstdout:\n%s", wantTmpfs, stdout)
	}
	// tmpfs must appear after the project bind mount.
	projectIdx := strings.Index(stdout, projectMount)
	tmpfsIdx := strings.Index(stdout, wantTmpfs)
	if projectIdx >= 0 && tmpfsIdx >= 0 && tmpfsIdx <= projectIdx {
		t.Errorf("tmpfs mount (byte %d) must come after project bind mount (byte %d)", tmpfsIdx, projectIdx)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "tmpfs") && strings.Contains(line, "source=") {
			t.Errorf("tmpfs mount line must not contain source=: %q", line)
		}
	}
}

// Absent .makeslop.yaml must yield the plain bridge-default BuildSpec output.
func TestRun_YamlAbsentIsBitIdenticalArgv(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	if err := os.Remove(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	// Absent yaml ⇒ both cache groups default to true.
	want := docker.BuildSpec(docker.Options{
		Projects: []docker.Project{{
			Host:  resolvedPwd,
			Name:  filepath.Base(workspaceDir),
			Label: "project: " + resolvedPwd,
		}},
		WorkspaceHost:     workspaceDir,
		BaseDir:           baseDir,
		Image:             s.Image,
		Command:           s.Shell,
		TmpDirSize:        s.TmpDirSize,
		MountContentCache: true,
		MountAgentCache:   true,
	}).ShellCommand()

	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("--dry-run stdout mismatch (yaml absent must yield bridge-default command)\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRun_YamlDedupsAgainstScan(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	envFile := filepath.Join(resolvedPwd, ".env")
	if err := os.WriteFile(envFile, []byte("S=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// Scan and explicit files both match .env; mergeUniqueSorted must dedup to one mount.
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  dirs: []\n  files: [.env]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	wantMount := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	count := strings.Count(stdout, wantMount)
	if count != 1 {
		t.Errorf("expected exactly 1 /dev/null mount for .env in --dry-run output, got %d\nstdout:\n%s", count, stdout)
	}
}

// ── YAML error propagation tests ──────────────────────────────────────────────

// Docker must never start when yaml parse or validation fails (secret-masking
// invariant). runWithExitCode (not runCmd) so non-errSilent errors land on stderr.
func TestRun_YamlMalformedAbortsBeforeDocker(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantFrag string // "" = only the prefix is checked
	}{
		{name: "malformed yaml", yaml: "exclude:\n  dirs: [unclosed\n"},
		{
			name:     "old flat environments form",
			yaml:     "environments:\n  NODE_ENV: production\n",
			wantFrag: `flat "KEY: value" form is no longer supported; move entries under environments.static`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setHomeToTestParent(t)
			baseDir := t.TempDir()
			pwd := t.TempDir()
			t.Chdir(pwd)

			initWithImage(t, baseDir)
			resolvedPwd := evalSymlinks(t, pwd)

			if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write bad yaml: %v", err)
			}

			fc := newFakeDocker(0, true)

			var stdout, stderr bytes.Buffer
			code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
			if code == 0 {
				t.Fatalf("expected non-zero exit from bad yaml, got 0; stderr=%q", stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "makeslop: ") {
				t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantFrag) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantFrag)
			}
			if fc.Started {
				t.Errorf("docker client must not be started on yaml error")
			}
		})
	}
}

// Uses runWithExitCode (not runCmd) so the error appears on stderr.
func TestRun_YamlReservedPathAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	yamlContent := "exclude:\n  dirs: [.claude]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	fc := newFakeDocker(0, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from reserved path, got 0; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reserved agent path") {
		t.Errorf("stderr missing 'reserved agent path': %q", stderr.String())
	}
	if fc.Started {
		t.Errorf("docker client must not be started when yaml lists reserved path")
	}
}

// Uses runWithExitCode (not runCmd) so the error appears on stderr.
func TestRun_YamlDirAndFileDupAborts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	yamlContent := "exclude:\n  dirs: [data]\n  files: [data]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	fc := newFakeDocker(0, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from cross-list dup, got 0; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "listed in both") {
		t.Errorf("stderr missing 'listed in both': %q", stderr.String())
	}
	if fc.Started {
		t.Errorf("docker client must not be started when yaml has cross-list dup")
	}
}

// A stale "network:" block (from the removed proxy feature) must abort `run` —
// the intended loud break forcing users to drop it on upgrade.
func TestRun_StaleNetworkBlockAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	staleYAML := "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: 10.0.0.5:3128\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(staleYAML), 0o644); err != nil {
		t.Fatalf("write stale yaml: %v", err)
	}

	fc := newFakeDocker(0, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from stale network: block, got 0; stderr=%q", stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "makeslop: ") {
		t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
	}
	if fc.Started {
		t.Errorf("docker client must not be started when yaml has stale network: block")
	}
}

func TestRun_YamlMissingPathSkippedSilently(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	yamlContent := "exclude:\n  dirs: []\n  files: [secrets/api.key]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	_ = os.Remove(filepath.Join(resolvedPwd, "secrets", "api.key"))

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("expected success when missing path is silently skipped, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "api.key") {
		t.Errorf("stderr must not mention missing path 'api.key': %q", stderr)
	}
	if strings.Contains(stdout, "api.key") {
		t.Errorf("--dry-run output must not contain overlay for missing api.key: %q", stdout)
	}
}

// Default is plain bridge networking: no --network, no HTTP_PROXY, no proxy volume.
func TestRun_DryRun_DefaultIsBridge(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	_ = os.Remove(filepath.Join(resolvedPwd, projectconfig.Filename))

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	if strings.Contains(stdout, "--network") {
		t.Errorf("default: stdout must not contain --network\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "HTTP_PROXY") {
		t.Errorf("default: stdout must not contain HTTP_PROXY\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "HTTPS_PROXY") {
		t.Errorf("default: stdout must not contain HTTPS_PROXY\nstdout:\n%s", stdout)
	}
	// Absent yaml ⇒ both cache groups default to true.
	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	want := docker.BuildSpec(docker.Options{
		Projects: []docker.Project{{
			Host:  resolvedPwd,
			Name:  filepath.Base(workspaceDir),
			Label: "project: " + resolvedPwd,
		}},
		WorkspaceHost:     workspaceDir,
		BaseDir:           baseDir,
		Image:             s.Image,
		Command:           s.Shell,
		TmpDirSize:        s.TmpDirSize,
		MountContentCache: true,
		MountAgentCache:   true,
	}).ShellCommand()
	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("default (bridge) stdout mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRun_DryRunIncludesMaskedDirs(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	resolvedPwd := evalSymlinks(t, pwd)

	secretsDir := filepath.Join(resolvedPwd, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}

	yamlContent := "exclude:\n  dirs: [secrets]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	wantFragment := "type=tmpfs,target=/workspace/" + name + "/secrets"
	if !strings.Contains(stdout, wantFragment) {
		t.Errorf("--dry-run stdout missing tmpfs mount: want substring %q\nstdout:\n%s", wantFragment, stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "tmpfs") && strings.Contains(line, "source=") {
			t.Errorf("tmpfs mount line must not contain source=: %q", line)
		}
	}
}

// ── Daemon/image preflight tests ──────────────────────────────────────────────

// Unreachable daemon: run aborts with a remedy and never starts the container.
func TestRun_DaemonDown_AbortsWithRemedy(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, true)
	fc.PingErr = errors.New("connection refused")

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when daemon is down, got nil; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (tailored message written to stderr), got %v", err)
	}
	if !strings.Contains(stderr, "is docker running") {
		t.Errorf("stderr missing 'is docker running' remedy: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started when daemon is unreachable")
	}
}

func TestRun_ImageMissing_AbortsWithRemedy(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, true)
	fc.ImageMissing = true

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when image is missing, got nil; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (tailored message written to stderr), got %v", err)
	}
	if !strings.Contains(stderr, `image "test-img" not found locally`) {
		t.Errorf("stderr missing 'not found locally': %q", stderr)
	}
	if !strings.Contains(stderr, "build or pull it (e.g. 'docker pull test-img')") {
		t.Errorf("stderr missing 'docker pull' remedy: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started when image is missing")
	}
}

func TestRun_ImageOtherError_PropagatesError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, true)
	fc.ImageErr = errors.New("permission denied reading image store")

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when ImageExists returns other-error, got nil; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("image other-error should return errSilent (message printed to stderr); got %v", err)
	}
	if !strings.Contains(stderr, "is docker running?") {
		t.Errorf("image other-error must emit 'is docker running?' hint; stderr=%q", stderr)
	}
	if strings.Contains(stderr, "not found locally") {
		t.Errorf("image other-error must not emit 'not found locally' hint; stderr=%q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started when ImageExists returns error")
	}
}

// --dry-run skips the daemon and image pre-flight checks. PingErr and
// ImageMissing are both set to confirm neither is consulted.
func TestRun_DryRun_SkipsDaemonAndImageCheck(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, false)
	fc.PingErr = errors.New("connection refused")
	fc.ImageMissing = true

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run must succeed even when daemon is down and image is missing; err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("--dry-run must print the command to stdout; got empty")
	}
	if fc.Started {
		t.Errorf("docker container must not be started on --dry-run")
	}
}

// Happy path: daemon ok, image present, workspace registered → container starts.
func TestRun_HappyPath_LaunchesDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run must succeed when daemon is ok and image exists; err=%v; stderr=%q", err, stderr)
	}
	if !fc.Started {
		t.Error("docker container must be started on happy path")
	}
}

// ── Cache mount overlay tests ──────────────────────────────────────────────────

// cache:{content:false,agent:false} drops all per-workspace cache mounts; global
// mounts and the project bind stay.
func TestRun_DryRun_CacheDisabled(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)

	resolvedPwd := evalSymlinks(t, pwd)
	yamlContent := "cache:\n  content: false\n  agent: false\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := "/workspace/" + workspaceName
	// Per-workspace mounts must be absent.
	if strings.Contains(stdout, workspacePath+"/.claude/") {
		t.Errorf("agent .claude/ mount must be absent when agent cache disabled; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, workspacePath+"/.codex/") {
		t.Errorf("agent .codex/ mount must be absent when agent cache disabled; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, workspacePath+"/docs/") {
		t.Errorf("content docs/ mount must be absent when content cache disabled; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, workspacePath+"/CLAUDE.md") {
		t.Errorf("content CLAUDE.md mount must be absent when content cache disabled; stdout:\n%s", stdout)
	}
	// Global mounts must still be present.
	if !strings.Contains(stdout, "target=/home/user/.claude/") {
		t.Errorf("global .claude/ mount must be present; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "target=/home/user/.claude.json") {
		t.Errorf("global .claude.json mount must be present; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "target=/home/user/.codex/") {
		t.Errorf("global .codex/ mount must be present; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "source="+resolvedPwd+",target="+workspacePath) {
		t.Errorf("project root bind must be present; stdout:\n%s", stdout)
	}
}

// Absent cache: block keeps all per-workspace cache mounts present (default = true).
func TestRun_DryRun_CacheDefault(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)

	resolvedPwd := evalSymlinks(t, pwd)
	yamlContent := "exclude:\n  scan:\n    patterns: []\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := "/workspace/" + workspaceName
	workspaceHost := filepath.Join(baseDir, "workspaces", workspaceName)

	wantAgentClaude := "source=" + filepath.Join(workspaceHost, ".claude") + "/,target=" + workspacePath + "/.claude/"
	if !strings.Contains(stdout, wantAgentClaude) {
		t.Errorf("agent .claude/ mount must be present by default; stdout:\n%s", stdout)
	}
	wantAgentCodex := "source=" + filepath.Join(workspaceHost, ".codex") + "/,target=" + workspacePath + "/.codex/"
	if !strings.Contains(stdout, wantAgentCodex) {
		t.Errorf("agent .codex/ mount must be present by default; stdout:\n%s", stdout)
	}
	wantDocs := "source=" + filepath.Join(workspaceHost, "docs") + "/,target=" + workspacePath + "/docs/"
	if !strings.Contains(stdout, wantDocs) {
		t.Errorf("content docs/ mount must be present by default; stdout:\n%s", stdout)
	}
	wantClaude := "source=" + filepath.Join(workspaceHost, "CLAUDE.md") + ",target=" + workspacePath + "/CLAUDE.md"
	if !strings.Contains(stdout, wantClaude) {
		t.Errorf("content CLAUDE.md mount must be present by default; stdout:\n%s", stdout)
	}
}

// cache:{content:false} keeps agent mounts but drops content mounts. Guards
// against a runRun wiring bug that swaps the Content/Agent assignments.
func TestRun_DryRun_CacheMixed(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)

	// content cache off; agent cache defaults to true.
	resolvedPwd := evalSymlinks(t, pwd)
	yamlContent := "cache:\n  content: false\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := "/workspace/" + workspaceName
	workspaceHost := filepath.Join(baseDir, "workspaces", workspaceName)

	// Content mounts absent (content=false).
	if strings.Contains(stdout, workspacePath+"/docs/") {
		t.Errorf("content docs/ must be absent when content cache disabled; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, workspacePath+"/CLAUDE.md") {
		t.Errorf("content CLAUDE.md must be absent when content cache disabled; stdout:\n%s", stdout)
	}
	// Agent mounts present (agent defaults to true).
	wantAgentClaude := "source=" + filepath.Join(workspaceHost, ".claude") + "/,target=" + workspacePath + "/.claude/"
	if !strings.Contains(stdout, wantAgentClaude) {
		t.Errorf("agent .claude/ mount must be present (agent=true); stdout:\n%s", stdout)
	}
	wantAgentCodex := "source=" + filepath.Join(workspaceHost, ".codex") + "/,target=" + workspacePath + "/.codex/"
	if !strings.Contains(stdout, wantAgentCodex) {
		t.Errorf("agent .codex/ mount must be present (agent=true); stdout:\n%s", stdout)
	}
}

// tmp_dir_size from settings.json threads into the docker run argv.
func TestRun_CustomTmpDirSize_FlowsIntoDockerArgv(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	s.TmpDirSize = "1000m"
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("makeslop go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stdout, "/tmp:size=1000m") {
		t.Errorf("--dry-run output missing '--tmpfs /tmp:size=1000m'; stdout:\n%s", stdout)
	}
}

// ── Environments, sandbox mounts, and quiet/symlink tests ─────────────────────

// dryRunEnvValues returns the -e values of a --dry-run ShellCommand in order.
// Test values are shell-safe, so tokens are printed unquoted.
func dryRunEnvValues(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), " \\")
		if v, ok := strings.CutPrefix(line, "-e "); ok {
			out = append(out, v)
		}
	}
	return out
}

// An environments: block flows into -e KEY=VALUE flags, sorted by key, in the
// dry-run output.
func TestRun_EnvironmentsBlock_ProducesEnvFlags(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)

	resolvedPwd := evalSymlinks(t, pwd)

	yamlContent := "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\nenvironments:\n  static:\n    NODE_ENV: production\n    PORT: \"8080\"\n    DEBUG: \"false\"\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	want := []string{"DEBUG=false", "NODE_ENV=production", "PORT=8080"}
	if got := dryRunEnvValues(stdout); !slices.Equal(got, want) {
		t.Errorf("--dry-run -e values = %q, want %q\nstdout:\n%s", got, want, stdout)
	}

	// Workspace mount must reference the registered workspace dir.
	wantWorkspaceMount := "/workspace/" + filepath.Base(workspaceDir)
	if !strings.Contains(stdout, wantWorkspaceMount) {
		t.Errorf("--dry-run stdout missing workspace mount %q\nstdout:\n%s", wantWorkspaceMount, stdout)
	}
}

// Absent environments: block produces no -e flags (backward compatibility).
func TestRun_NoEnvironmentsBlock_NoEnvFlags(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	for _, tok := range strings.Fields(stdout) {
		if tok == "-e" {
			t.Errorf("--dry-run output must not contain -e flags when environments: block is absent\nstdout:\n%s", stdout)
			break
		}
	}
}

func TestResolveEnv(t *testing.T) {
	hostEnv := map[string]string{
		"SET_VAR":   "val",
		"EMPTY_VAR": "",
		"PEM_KEY":   "line1\nline2\n",
		"AAA_FIRST": "1",
		"PORT":      "80",
	}
	lookup := func(name string) (string, bool) {
		v, ok := hostEnv[name]
		return v, ok
	}

	tests := []struct {
		name string
		env  projectconfig.Env
		want []string
	}{
		{name: "empty env", env: projectconfig.Env{}, want: nil},
		{name: "only unset host names", env: projectconfig.Env{Host: []string{"UNSET_A", "UNSET_B"}}, want: nil},
		{name: "set host var", env: projectconfig.Env{Host: []string{"SET_VAR"}}, want: []string{"SET_VAR=val"}},
		{name: "set-empty host var", env: projectconfig.Env{Host: []string{"EMPTY_VAR"}}, want: []string{"EMPTY_VAR="}},
		{name: "unset host var skipped", env: projectconfig.Env{Host: []string{"SET_VAR", "UNSET_VAR"}}, want: []string{"SET_VAR=val"}},
		{name: "static only, file order sorted", env: projectconfig.Env{Static: []string{"B=2", "A=1"}}, want: []string{"A=1", "B=2"}},
		{
			name: "sorted merge across static and host",
			env:  projectconfig.Env{Static: []string{"MID=m", "ZED=z"}, Host: []string{"AAA_FIRST", "SET_VAR"}},
			want: []string{"AAA_FIRST=1", "MID=m", "SET_VAR=val", "ZED=z"},
		},
		{
			// Sorting full "KEY=VALUE" strings would put PORT2 first ('2' < '=').
			name: "sorted by key, not by pair",
			env:  projectconfig.Env{Static: []string{"PORT2=x"}, Host: []string{"PORT"}},
			want: []string{"PORT=80", "PORT2=x"},
		},
		{
			name: "host value with newline passed verbatim",
			env:  projectconfig.Env{Host: []string{"PEM_KEY"}},
			want: []string{"PEM_KEY=line1\nline2\n"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEnv(tc.env, lookup)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("resolveEnv = %#v, want nil", got)
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("resolveEnv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// resolveEnv must not reorder or write through env.Static (spare capacity
// included) and must look up only env.Host names.
func TestResolveEnv_NoAliasingAndLooksUpOnlyHost(t *testing.T) {
	backing := make([]string, 2, 8)
	backing[0], backing[1] = "Z=1", "A=2"
	env := projectconfig.Env{Static: backing, Host: []string{"H1", "H2"}}

	var looked []string
	lookup := func(name string) (string, bool) {
		looked = append(looked, name)
		return "v", true
	}

	got := resolveEnv(env, lookup)

	if want := []string{"A=2", "H1=v", "H2=v", "Z=1"}; !slices.Equal(got, want) {
		t.Errorf("resolveEnv = %q, want %q", got, want)
	}
	if want := []string{"Z=1", "A=2"}; !slices.Equal(env.Static, want) {
		t.Errorf("env.Static mutated: %q, want %q", env.Static, want)
	}
	if spare := backing[:4]; spare[2] != "" || spare[3] != "" {
		t.Errorf("resolveEnv wrote into env.Static spare capacity: %q", spare)
	}
	if want := []string{"H1", "H2"}; !slices.Equal(looked, want) {
		t.Errorf("lookup called with %q, want %q", looked, want)
	}
}

// environments.host names are resolved from the host environment and printed in --dry-run.
func TestRun_EnvironmentsHost_DryRunResolvesValues(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	const unsetName = "MAKESLOP_TEST_UNSET_VAR_7F3A"
	t.Setenv("MAKESLOP_TEST_SET_VAR", "val")
	t.Setenv("MAKESLOP_TEST_EMPTY_VAR", "")
	t.Setenv(unsetName, "") // registers restore; t.Setenv cannot unset by itself
	if err := os.Unsetenv(unsetName); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	yamlContent := "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\n" +
		"environments:\n  static:\n    MAKESLOP_TEST_STATIC: s\n  host:\n" +
		"    - MAKESLOP_TEST_SET_VAR\n    - " + unsetName + "\n    - MAKESLOP_TEST_EMPTY_VAR\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	// Sorted: EMPTY < SET < STATIC; the unset name is absent.
	want := []string{"MAKESLOP_TEST_EMPTY_VAR=", "MAKESLOP_TEST_SET_VAR=val", "MAKESLOP_TEST_STATIC=s"}
	if got := dryRunEnvValues(stdout); !slices.Equal(got, want) {
		t.Errorf("--dry-run -e values = %q, want %q\nstdout:\n%s", got, want, stdout)
	}
	if strings.Contains(stdout, unsetName) {
		t.Errorf("--dry-run stdout must not mention unset host var %q\nstdout:\n%s", unsetName, stdout)
	}
}

// A real run hands the runner the same resolved, sorted pairs dry-run prints.
func TestRun_EnvironmentsHost_SpecEnvResolved(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	const unsetName = "MAKESLOP_TEST_UNSET_VAR_9B2C"
	t.Setenv("MAKESLOP_TEST_SET_VAR", "line1\nline2")
	t.Setenv(unsetName, "")
	if err := os.Unsetenv(unsetName); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	yamlContent := "environments:\n  static:\n    ZZ_STATIC: z\n    AA_STATIC: a\n  host:\n" +
		"    - MAKESLOP_TEST_SET_VAR\n    - " + unsetName + "\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	fc := newFakeDocker(0, true)
	if _, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}

	want := []string{"AA_STATIC=a", "MAKESLOP_TEST_SET_VAR=line1\nline2", "ZZ_STATIC=z"}
	if !slices.Equal(fc.LastSpec.Env, want) {
		t.Errorf("spec Env = %q, want %q", fc.LastSpec.Env, want)
	}
}

// Git project + .makeslop.yaml present → both sandbox mounts in the spec
// the fake runner receives.
func TestRun_GitAndConfig_BothSandboxMounts(t *testing.T) {
	skipNonPOSIX(t, "symlinks/Lstat behaviour is POSIX-specific")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Create .git as a directory (git project gate).
	if err := os.MkdirAll(filepath.Join(resolvedPwd, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir .git/hooks: %v", err)
	}
	// .makeslop.yaml was created by init (regular file). No need to re-create.

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if !fc.Started {
		t.Fatal("docker.Run was not invoked")
	}

	workspacePath := "/workspace/" + workspaceName
	wantConfigContainer := workspacePath + "/.makeslop.yaml"
	wantHooksContainer := workspacePath + "/.git/hooks"

	if !hasMountWithContainerAndHost(fc.LastSpec.Mounts, wantConfigContainer, filepath.Join(resolvedPwd, ".makeslop.yaml")) {
		t.Errorf("ProtectProjectConfig mount not found in spec\nmounts: %+v", fc.LastSpec.Mounts)
	}
	// Verify read-only flag.
	for _, m := range fc.LastSpec.Mounts {
		if m.Container == wantConfigContainer && !m.ReadOnly {
			t.Errorf("ProtectProjectConfig mount must be read-only; got ReadOnly=false")
		}
	}

	if !hasMountWithContainer(fc.LastSpec.Mounts, wantHooksContainer) {
		t.Errorf("MaskGitHooks tmpfs mount not found in spec\nmounts: %+v", fc.LastSpec.Mounts)
	}
	// Verify tmpfs type.
	for _, m := range fc.LastSpec.Mounts {
		if m.Container == wantHooksContainer && m.Type != "tmpfs" {
			t.Errorf("MaskGitHooks mount type = %q, want %q", m.Type, "tmpfs")
		}
	}
}

// No .git directory → MaskGitHooks mount absent.
func TestRun_NoGit_NoHooksMask(t *testing.T) {
	skipNonPOSIX(t, "symlinks/Lstat behaviour is POSIX-specific")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Ensure .git does NOT exist.
	_ = os.RemoveAll(filepath.Join(resolvedPwd, ".git"))

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if !fc.Started {
		t.Fatal("docker.Run must have been invoked (fc.Started must be true)")
	}

	workspacePath := "/workspace/" + workspaceName
	wantHooksContainer := workspacePath + "/.git/hooks"
	if hasMountWithContainer(fc.LastSpec.Mounts, wantHooksContainer) {
		t.Errorf("MaskGitHooks mount must be absent when .git does not exist; mounts: %+v", fc.LastSpec.Mounts)
	}
}

// No .makeslop.yaml → ProtectProjectConfig mount absent.
func TestRun_NoConfig_NoConfigMount(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Remove .makeslop.yaml so the gate is off.
	if err := os.Remove(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove .makeslop.yaml: %v", err)
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if !fc.Started {
		t.Fatal("docker.Run must have been invoked (fc.Started must be true)")
	}

	workspacePath := "/workspace/" + workspaceName
	wantConfigContainer := workspacePath + "/.makeslop.yaml"
	if hasMountWithContainer(fc.LastSpec.Mounts, wantConfigContainer) {
		t.Errorf("ProtectProjectConfig mount must be absent when .makeslop.yaml does not exist; mounts: %+v", fc.LastSpec.Mounts)
	}
}

// .git as a regular file (worktree/submodule gitfile) → MaskGitHooks mount absent.
func TestRun_GitFile_NoHooksMask(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Simulate a gitfile (worktree/submodule): .git is a regular file.
	if err := os.WriteFile(filepath.Join(resolvedPwd, ".git"), []byte("gitdir: ../.git/worktrees/main\n"), 0o644); err != nil {
		t.Fatalf("write .git gitfile: %v", err)
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if !fc.Started {
		t.Fatal("docker.Run must have been invoked (fc.Started must be true)")
	}

	workspacePath := "/workspace/" + workspaceName
	wantHooksContainer := workspacePath + "/.git/hooks"
	if hasMountWithContainer(fc.LastSpec.Mounts, wantHooksContainer) {
		t.Errorf("MaskGitHooks mount must be absent when .git is a regular file (gitfile); mounts: %+v", fc.LastSpec.Mounts)
	}
}

// Quiet-contract: --quiet suppresses "masked N" chrome but NOT symlink warnings.
func TestRun_QuietContract_SuppressesMaskedButNotSymlinkWarnings(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Create a real .env so the scan finds 1 match for the chrome line.
	if err := os.WriteFile(filepath.Join(resolvedPwd, "real.env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write real.env: %v", err)
	}
	// Create a symlink .env → real.env so the scan finds a symlink match.
	if err := os.Symlink("real.env", filepath.Join(resolvedPwd, "symlink.env")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Non-quiet: both "masked" chrome AND symlink warning appear.
	_, stderrNonQuiet, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("non-quiet --dry-run failed: %v; stderr=%q", err, stderrNonQuiet)
	}
	if !strings.Contains(stderrNonQuiet, "masked 1 secret file") {
		t.Errorf("non-quiet: stderr must contain 'masked 1 secret file'; got: %q", stderrNonQuiet)
	}
	wantSymlinkWarning := "makeslop: warning: symlink symlink.env matches a secret pattern but is NOT masked"
	if !strings.Contains(stderrNonQuiet, wantSymlinkWarning) {
		t.Errorf("non-quiet: stderr must contain symlink warning %q; got: %q", wantSymlinkWarning, stderrNonQuiet)
	}

	// Quiet: "masked" chrome IS suppressed; symlink warning is NOT.
	_, stderrQuiet, err := runCmd(t, baseDir, "--quiet", "run", "--dry-run")
	if err != nil {
		t.Fatalf("quiet --dry-run failed: %v; stderr=%q", err, stderrQuiet)
	}
	if strings.Contains(stderrQuiet, "masked 1 secret file") {
		t.Errorf("--quiet: 'masked N' chrome must be suppressed; stderr=%q", stderrQuiet)
	}
	if !strings.Contains(stderrQuiet, wantSymlinkWarning) {
		t.Errorf("--quiet: symlink warning must NOT be suppressed; got: %q", stderrQuiet)
	}
}

// --dry-run output includes the sandbox-policy mounts when gates are on.
func TestRun_DryRun_SandboxMountsPresent(t *testing.T) {
	skipNonPOSIX(t, "symlinks/Lstat behaviour is POSIX-specific")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut := initWithImage(t, baseDir)
	workspaceDir := strings.TrimSpace(initOut)
	workspaceName := filepath.Base(workspaceDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Create .git directory to activate MaskGitHooks.
	if err := os.MkdirAll(filepath.Join(resolvedPwd, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir .git/hooks: %v", err)
	}
	// .makeslop.yaml already created by init.

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := "/workspace/" + workspaceName

	// ProtectProjectConfig: read-only bind of .makeslop.yaml.
	wantConfigMount := "type=bind,source=" + filepath.Join(resolvedPwd, ".makeslop.yaml") +
		",target=" + workspacePath + "/.makeslop.yaml,readonly"
	if !strings.Contains(stdout, wantConfigMount) {
		t.Errorf("--dry-run stdout missing ProtectProjectConfig mount\nwant substring: %q\nstdout:\n%s",
			wantConfigMount, stdout)
	}

	// MaskGitHooks: tmpfs on .git/hooks.
	wantHooksMount := "type=tmpfs,target=" + workspacePath + "/.git/hooks"
	if !strings.Contains(stdout, wantHooksMount) {
		t.Errorf("--dry-run stdout missing MaskGitHooks mount\nwant substring: %q\nstdout:\n%s",
			wantHooksMount, stdout)
	}
}

// Combined test: both warning sources (security.Scan symlinkMatches AND
// projectconfig Excludes.Warnings) fire together under --quiet. A regression
// that gates one path under quietWriter would silence one but not the other.
func TestRun_QuietContract_BothWarningSources(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Source 1 (security.Scan): a symlink whose basename matches a scan pattern.
	if err := os.WriteFile(filepath.Join(resolvedPwd, "real.env"), []byte("S=1"), 0o644); err != nil {
		t.Fatalf("write real.env: %v", err)
	}
	if err := os.Symlink("real.env", filepath.Join(resolvedPwd, "scan-link.env")); err != nil {
		t.Fatalf("create scan symlink: %v", err)
	}

	// Source 2 (projectconfig): a symlink listed explicitly in exclude.files.
	target := filepath.Join(resolvedPwd, "real.key")
	if err := os.WriteFile(target, []byte("K=1"), 0o644); err != nil {
		t.Fatalf("write real.key: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(resolvedPwd, "config-link.key")); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}

	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  files: [config-link.key]\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	wantScanWarn := "makeslop: warning: symlink scan-link.env matches a secret pattern but is NOT masked"
	wantConfigWarn := `makeslop: warning: path "config-link.key" is a symlink and is NOT masked`

	_, stderrQuiet, err := runCmd(t, baseDir, "--quiet", "run", "--dry-run")
	if err != nil {
		t.Fatalf("--quiet --dry-run failed: %v; stderr=%q", err, stderrQuiet)
	}
	if !strings.Contains(stderrQuiet, wantScanWarn) {
		t.Errorf("--quiet: scan symlink warning must NOT be suppressed\nwant: %q\ngot:  %q", wantScanWarn, stderrQuiet)
	}
	if !strings.Contains(stderrQuiet, wantConfigWarn) {
		t.Errorf("--quiet: projectconfig symlink warning must NOT be suppressed\nwant: %q\ngot:  %q", wantConfigWarn, stderrQuiet)
	}
}

// .makeslop.yaml-as-symlink: Load now rejects symlinks fail-loud (finding #2),
// so makeslop run must fail with a clear "is a symlink" error — docker.Run is
// never invoked (the symlink is caught before the daemon is contacted).
func TestRun_ConfigAsSymlink_FailsLoud(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Replace .makeslop.yaml with a live symlink pointing to a valid config file.
	realConfig := filepath.Join(resolvedPwd, ".makeslop.yaml.real")
	if err := os.Rename(filepath.Join(resolvedPwd, projectconfig.Filename), realConfig); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(realConfig, filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatal("expected run to fail when .makeslop.yaml is a symlink, got nil error")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(stderr, "symlink") {
		t.Errorf("expected error/stderr to mention 'symlink'; err=%v stderr=%q", err, stderr)
	}
	// docker.Run must NOT have been invoked — the symlink check fires before the daemon.
	if fc.Started {
		t.Error("docker.Run must NOT be invoked when .makeslop.yaml is a symlink")
	}
}

// projectconfig symlink warnings appear on stderr (bypassing --quiet).
func TestRun_ProjectconfigSymlinkWarning(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Create a symlink that will be listed in exclude.files.
	target := filepath.Join(resolvedPwd, "real_secret.key")
	if err := os.WriteFile(target, []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	symlinkPath := filepath.Join(resolvedPwd, "sym_secret.key")
	if err := os.Symlink(target, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// List the symlink in exclude.files — projectconfig.Load should produce a warning.
	yamlContent := "exclude:\n  scan:\n    patterns: []\n  files: [sym_secret.key]\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, stderrNonQuiet, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("non-quiet --dry-run failed: %v; stderr=%q", err, stderrNonQuiet)
	}
	if !strings.Contains(stderrNonQuiet, "symlink") {
		t.Errorf("non-quiet: stderr must contain projectconfig symlink warning; got: %q", stderrNonQuiet)
	}

	// --quiet must NOT suppress this warning.
	_, stderrQuiet, err := runCmd(t, baseDir, "--quiet", "run", "--dry-run")
	if err != nil {
		t.Fatalf("quiet --dry-run failed: %v; stderr=%q", err, stderrQuiet)
	}
	if !strings.Contains(stderrQuiet, "symlink") {
		t.Errorf("--quiet: projectconfig symlink warning must NOT be suppressed; got: %q", stderrQuiet)
	}
}

// Ordering test: daemon down + invalid .makeslop.yaml → daemon error is reported,
// not a YAML parse error. Proves that CheckDaemon fires before projectconfig.Load.
func TestRun_DaemonCheckedBeforeYamlParse(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)
	resolvedPwd := evalSymlinks(t, pwd)

	// Plant an invalid .makeslop.yaml so that a YAML parse error would occur
	// if projectconfig.Load were reached before CheckDaemon.
	badYAML := []byte("exclude:\n  dirs: [unclosed\n")
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), badYAML, 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}

	// Make the daemon unreachable.
	fc := newFakeDocker(0, true)
	fc.PingErr = errors.New("connection refused")

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when daemon is down, got nil; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (tailored message written to stderr), got %v", err)
	}
	// The daemon error must be reported — not a YAML parse error.
	if !strings.Contains(stderr, "is docker running") {
		t.Errorf("daemon-down error must be reported before YAML parse; stderr=%q", stderr)
	}
	// Must not see a YAML-related error (which would prove the wrong order).
	if strings.Contains(stderr, "yaml") || strings.Contains(stderr, "parse") || strings.Contains(stderr, "unmarshal") {
		t.Errorf("YAML parse error must not appear; daemon error should have fired first; stderr=%q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started when daemon is unreachable")
	}
}

// Regular .makeslop.yaml → protect=true.
func TestSandboxMountGates_RegularConfigFile_Protect(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, projectconfig.Filename)
	if err := os.WriteFile(cfgPath, []byte("# yaml"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	protect, maskHooks := sandboxMountGates(root)

	if !protect {
		t.Errorf("protect = false, want true (regular file)")
	}
	// .git absent → maskHooks = false
	if maskHooks {
		t.Errorf("maskHooks = true, want false (.git absent)")
	}
}

// Missing .makeslop.yaml → protect=false.
func TestSandboxMountGates_MissingConfig_NoProtect(t *testing.T) {
	root := t.TempDir()

	protect, _ := sandboxMountGates(root)

	if protect {
		t.Errorf("protect = true, want false (config absent)")
	}
}

// .git is a directory → maskHooks=true.
func TestSandboxMountGates_GitDir_MaskHooks(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	_, maskHooks := sandboxMountGates(root)

	if !maskHooks {
		t.Errorf("maskHooks = false, want true (.git is a directory)")
	}
}

// .git is a regular file (gitfile/worktree) → maskHooks=false.
func TestSandboxMountGates_GitFile_NoMaskHooks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("write gitfile: %v", err)
	}

	_, maskHooks := sandboxMountGates(root)

	if maskHooks {
		t.Errorf("maskHooks = true, want false (.git is a regular file / gitfile)")
	}
}

// exits 0 (daemon preflight is skipped on --dry-run).
func TestRun_DryRun_DaemonDown_StillPrints(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	// Daemon unreachable AND TTY false (typical in CI / go test).
	fc := newFakeDocker(0, false)
	fc.PingErr = errors.New("connection refused")

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run must succeed even when daemon is down; err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("--dry-run must print the docker run command to stdout; got empty")
	}
	if strings.Contains(stderr, "is docker running") {
		t.Errorf("--dry-run must not invoke daemon preflight; stderr=%q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started on --dry-run")
	}
}

// reportScanResults: masked count goes to chrome (quiet-suppressible); symlink
// warnings always go to stderr. Rel-failure fallback uses absolute path.
func TestReportScanResults_TwoWriterContract(t *testing.T) {
	var stderr, chrome bytes.Buffer
	root := "/some/root"
	masked := []string{"/some/root/a.env", "/some/root/b.env"}
	reportScanResults(&stderr, &chrome, root, false, masked, nil)

	if !strings.Contains(chrome.String(), "masked 2 secret file(s)") {
		t.Errorf("chrome missing masked count: %q", chrome.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr must be empty when no symlinks: %q", stderr.String())
	}
}

func TestReportScanResults_SymlinkWarningToStderr(t *testing.T) {
	var stderr, chrome bytes.Buffer
	root := "/some/root"
	syms := []string{"/some/root/link.env"}
	reportScanResults(&stderr, &chrome, root, false, nil, syms)

	if chrome.String() != "" {
		t.Errorf("chrome must be empty when no masked files: %q", chrome.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Errorf("stderr missing 'symlink' warning: %q", stderr.String())
	}
	// Relative path should appear (Rel succeeds here).
	if !strings.Contains(stderr.String(), "link.env") {
		t.Errorf("stderr missing symlink name: %q", stderr.String())
	}
}

func TestReportScanResults_RelFallbackToAbsolute(t *testing.T) {
	var stderr, chrome bytes.Buffer
	// Symlink not under root; filepath.Rel returns a dotdot path (no error on
	// POSIX), so the name still appears in the output.
	root := "/a/b/c"
	sym := "/x/y/z/link.env"
	reportScanResults(&stderr, &chrome, root, false, nil, []string{sym})

	// Either the relative or absolute path must appear in the warning.
	if !strings.Contains(stderr.String(), "link.env") {
		t.Errorf("stderr missing symlink name regardless of Rel fallback: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Errorf("stderr missing 'symlink' warning: %q", stderr.String())
	}
}

func TestRun_ImageFlag_OverridesSettings(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "-i", "flag-img:1")
	if err != nil {
		t.Fatalf("run -i failed: %v; stderr=%q", err, stderr)
	}
	if fc.LastSpec.Image != "flag-img:1" {
		t.Errorf("LastSpec.Image = %q, want %q", fc.LastSpec.Image, "flag-img:1")
	}
	if fc.ImageChecked != "flag-img:1" {
		t.Errorf("image preflight checked %q, want %q", fc.ImageChecked, "flag-img:1")
	}
}

func TestRun_ImageFlag_NoSettingsImage(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, stderr, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "-i", "flag-img")
	if err != nil {
		t.Fatalf("run -i failed: %v; stderr=%q", err, stderr)
	}
	if fc.LastSpec.Image != "flag-img" {
		t.Errorf("LastSpec.Image = %q, want %q", fc.LastSpec.Image, "flag-img")
	}
	if fc.ImageChecked != "flag-img" {
		t.Errorf("image preflight checked %q, want %q", fc.ImageChecked, "flag-img")
	}
}

func TestRun_WhitespaceImage_TreatedAsUnset(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, stderr, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}
	writeWhitespaceImage(t, baseDir)

	fc := newFakeDocker(0, true)
	if _, _, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); !errors.Is(err, errNoImage) {
		t.Fatalf("expected errNoImage, got %v", err)
	}
	if fc.DaemonChecked || fc.Started {
		t.Error("docker must not be called when the image is whitespace-only")
	}
}

func TestRun_ImageFlag_DryRunShowsOverride(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initWithImage(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run", "--image", "flag-img:1")
	if err != nil {
		t.Fatalf("run --dry-run --image failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "flag-img:1") {
		t.Errorf("--dry-run output missing flag image; stdout=%q", stdout)
	}
	if strings.Contains(stdout, "test-img") {
		t.Errorf("--dry-run output must not contain the settings image; stdout=%q", stdout)
	}
}

func TestRun_InvalidImageFlag_RejectedBeforeDocker(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--dry-run", "-i=--privileged"},
		{"run", "--image", "MyImage:latest"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setHomeToTestParent(t)
			baseDir := t.TempDir()
			t.Chdir(t.TempDir())
			initWithImage(t, baseDir)

			fc := newFakeDocker(0, true)
			stdout, _, err := runCmdWithDeps(t, baseDir, depsFrom(fc), args...)
			if err == nil || !strings.Contains(err.Error(), "invalid image reference") {
				t.Fatalf("err = %v, want invalid image reference", err)
			}
			if stdout != "" {
				t.Errorf("stdout must be empty; got %q", stdout)
			}
			if fc.DaemonChecked || fc.ImageChecked != "" || fc.Started {
				t.Error("docker must not be called for an invalid image reference")
			}
		})
	}
}

func TestRun_NoImage_FailsBeforeDocker(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"run", []string{"run"}},
		{"dry-run", []string{"run", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHomeToTestParent(t)
			baseDir := t.TempDir()
			pwd := t.TempDir()
			t.Chdir(pwd)

			if _, stderr, err := runCmd(t, baseDir, "init"); err != nil {
				t.Fatalf("init failed: %v; stderr=%q", err, stderr)
			}

			fc := newFakeDocker(0, true)

			stdout, _, err := runCmdWithDeps(t, baseDir, depsFrom(fc), tc.args...)
			if !errors.Is(err, errNoImage) {
				t.Fatalf("expected errNoImage, got %v", err)
			}
			if !strings.Contains(err.Error(), "config set image") {
				t.Errorf("error missing 'config set image' hint: %q", err.Error())
			}
			if stdout != "" {
				t.Errorf("stdout must be empty; got %q", stdout)
			}
			if fc.DaemonChecked {
				t.Error("daemon preflight must not run when no image is configured")
			}
			if fc.ImageChecked != "" || fc.Started {
				t.Error("image preflight and runner must not be called when no image is configured")
			}
		})
	}
}

func TestRun_NoImage_Unregistered_ImageErrorWins(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	fc := newFakeDocker(0, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if !errors.Is(err, errNoImage) {
		t.Fatalf("expected errNoImage, got %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "no workspace registered") {
		t.Errorf("workspace error must not be printed before the image error; stderr=%q", stderr)
	}
	if fc.DaemonChecked || fc.Started {
		t.Error("docker must not be touched")
	}
}

// ── Network preflight tests ───────────────────────────────────────────────────

// writeNetworkYaml writes a .makeslop.yaml in the (registered) cwd with the
// given network lines appended to an empty exclude block.
func writeNetworkYaml(t *testing.T, pwd, networkLines string) {
	t.Helper()
	content := "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\n" + networkLines
	if err := os.WriteFile(filepath.Join(evalSymlinks(t, pwd), projectconfig.Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
}

func setupNetworkRun(t *testing.T, networkLines string) string {
	t.Helper()
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)
	initWithImage(t, baseDir)
	writeNetworkYaml(t, pwd, networkLines)
	return baseDir
}

func TestRun_DryRun_NetworkContainer_NoDaemonCalls(t *testing.T) {
	baseDir := setupNetworkRun(t, "network_mode: \"container:proxy\"\n")
	fc := newFakeDocker(0, false)
	fc.PingErr = errors.New("connection refused")

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "--network container:proxy") {
		t.Errorf("dry-run stdout missing --network container:proxy:\n%s", stdout)
	}
	if fc.DaemonChecked || fc.ImageChecked != "" || len(fc.ContainersInspected) > 0 || len(fc.NetworksInspected) > 0 {
		t.Errorf("dry-run must make no daemon calls; daemon=%v image=%q containers=%v networks=%v",
			fc.DaemonChecked, fc.ImageChecked, fc.ContainersInspected, fc.NetworksInspected)
	}
}

func TestRun_NetworkPreflight(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		containers     map[string]bool
		networks       map[string]bool
		containerErr   error
		networkErr     error
		wantErr        []string // substrings of the returned error; nil = success
		wantContainers []string
		wantNetworks   []string
	}{
		{
			name:           "container missing",
			yaml:           "network_mode: \"container:proxy\"\n",
			wantErr:        []string{`network_mode: container "proxy" not found — start it first`, "compose names containers", "docker ps"},
			wantContainers: []string{"proxy"},
		},
		{
			name:           "container stopped",
			yaml:           "network_mode: \"container:proxy\"\n",
			containers:     map[string]bool{"proxy": false},
			wantErr:        []string{`network_mode: container "proxy" is not running (stopped, paused or restarting) — start or unpause it`},
			wantContainers: []string{"proxy"},
		},
		{
			name:           "container running",
			yaml:           "network_mode: \"container:proxy\"\n",
			containers:     map[string]bool{"proxy": true},
			wantContainers: []string{"proxy"},
		},
		{
			name:           "container inspect error",
			yaml:           "network_mode: \"container:proxy\"\n",
			containerErr:   errors.New("boom"),
			wantErr:        []string{`network_mode: check container "proxy": boom`},
			wantContainers: []string{"proxy"},
		},
		{
			name:         "network missing",
			yaml:         "networks: [a, b]\n",
			networks:     map[string]bool{"a": true},
			wantErr:      []string{`networks: network "b" not found — create it with 'docker network create b'`, "<project>_", "docker network ls"},
			wantNetworks: []string{"a", "b"},
		},
		{
			name:         "networks present",
			yaml:         "networks: [a, b]\n",
			networks:     map[string]bool{"a": true, "b": true},
			wantNetworks: []string{"a", "b"},
		},
		{
			name:         "network inspect error",
			yaml:         "networks: [a]\n",
			networkErr:   errors.New("boom"),
			wantErr:      []string{`networks: check network "a": boom`},
			wantNetworks: []string{"a"},
		},
		{
			name:         "custom network_mode inspected as network",
			yaml:         "network_mode: myapp_default\n",
			networks:     map[string]bool{"myapp_default": true},
			wantNetworks: []string{"myapp_default"},
		},
		{
			name:         "custom network_mode missing",
			yaml:         "network_mode: myapp_default\n",
			wantErr:      []string{`network_mode: network "myapp_default" not found`},
			wantNetworks: []string{"myapp_default"},
		},
		{name: "unset", yaml: ""},
		{name: "bridge", yaml: "network_mode: bridge\n"},
		{name: "host", yaml: "network_mode: host\n"},
		{name: "none", yaml: "network_mode: none\n"},
		{name: "default", yaml: "network_mode: default\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDir := setupNetworkRun(t, tt.yaml)
			fc := newFakeDocker(0, true)
			fc.Containers = tt.containers
			fc.Networks = tt.networks
			fc.ContainerErr = tt.containerErr
			fc.NetworkErr = tt.networkErr

			_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
			if tt.wantErr != nil {
				if err == nil || errors.Is(err, errSilent) {
					t.Fatalf("want a printable error, got %v; stderr=%q", err, stderr)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error missing %q: %v", s, err)
					}
				}
				if strings.Contains(err.Error(), "is docker running?") {
					t.Errorf("inspect errors must not blame the daemon: %v", err)
				}
				if fc.Started {
					t.Error("container must not start when network preflight fails")
				}
			} else {
				if err != nil {
					t.Fatalf("run failed: %v; stderr=%q", err, stderr)
				}
				if !fc.Started {
					t.Error("container must start when network preflight passes")
				}
			}
			if fc.NetworkCtxNoDeadline {
				t.Error("network inspects must run under the preflight timeout")
			}
			if !slices.Equal(fc.ContainersInspected, tt.wantContainers) {
				t.Errorf("containers inspected = %q, want %q", fc.ContainersInspected, tt.wantContainers)
			}
			if !slices.Equal(fc.NetworksInspected, tt.wantNetworks) {
				t.Errorf("networks inspected = %q, want %q", fc.NetworksInspected, tt.wantNetworks)
			}
		})
	}
}

// The network settings flow from .makeslop.yaml into the executed Spec.
func TestRun_NetworkFlowsIntoSpec(t *testing.T) {
	baseDir := setupNetworkRun(t, "networks: [a, b]\n")
	fc := newFakeDocker(0, true)
	fc.Networks = map[string]bool{"a": true, "b": true}

	if _, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if fc.LastSpec.NetworkMode != "" || !slices.Equal(fc.LastSpec.Networks, []string{"a", "b"}) {
		t.Errorf("spec network = (%q, %q), want (\"\", [a b])", fc.LastSpec.NetworkMode, fc.LastSpec.Networks)
	}
	if got := string(fc.LastSpec.HostConfig().NetworkMode); got != "a" {
		t.Errorf("HostConfig.NetworkMode = %q, want a", got)
	}
}

// Acceptance: network_mode "container:proxy" reaches the executed HostConfig
// with no NetworkingConfig, and matches what --dry-run prints.
func TestRun_NetworkContainerProxy_DryRunMatchesExecuted(t *testing.T) {
	baseDir := setupNetworkRun(t, "network_mode: \"container:proxy\"\n")

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(newFakeDocker(0, false)), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	m := regexp.MustCompile(`--network (\S+)`).FindAllStringSubmatch(stdout, -1)
	if len(m) != 1 {
		t.Fatalf("dry-run stdout: want exactly one --network flag, got %d:\n%s", len(m), stdout)
	}
	printed := m[0][1]
	if printed != "container:proxy" {
		t.Errorf("printed --network %q, want container:proxy", printed)
	}

	fc := newFakeDocker(0, true)
	fc.Containers = map[string]bool{"proxy": true}
	if _, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if got := string(fc.LastSpec.HostConfig().NetworkMode); got != printed {
		t.Errorf("executed HostConfig.NetworkMode = %q, printed --network %q", got, printed)
	}
	if nc := fc.LastSpec.NetworkingConfig(); nc != nil {
		t.Errorf("NetworkingConfig = %v, want nil for container mode", nc)
	}
}

// Acceptance: with no network keys, dry-run carries no --network flag and the
// executed spec leaves Docker's default network in place.
func TestRun_NoNetworkKeys_NoNetworkFlag(t *testing.T) {
	baseDir := setupNetworkRun(t, "")

	stdout, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(newFakeDocker(0, false)), "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stdout, "--network") {
		t.Errorf("dry-run stdout must not contain --network with no keys:\n%s", stdout)
	}

	fc := newFakeDocker(0, true)
	if _, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run"); err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	if got := fc.LastSpec.HostConfig().NetworkMode; got != "" {
		t.Errorf("HostConfig.NetworkMode = %q, want empty", got)
	}
	if nc := fc.LastSpec.NetworkingConfig(); nc != nil {
		t.Errorf("NetworkingConfig = %v, want nil", nc)
	}
}

// The image check runs before the network check.
func TestRun_NetworkPreflight_AfterImageCheck(t *testing.T) {
	baseDir := setupNetworkRun(t, "network_mode: \"container:proxy\"\n")
	fc := newFakeDocker(0, true)
	fc.ImageMissing = true

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if !errors.Is(err, errSilent) {
		t.Fatalf("want errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not found locally") {
		t.Errorf("stderr missing image hint: %q", stderr)
	}
	if len(fc.ContainersInspected) > 0 {
		t.Errorf("network preflight must not run when image is missing; inspected %q", fc.ContainersInspected)
	}
}

// Invalid network config fails run with the validation error before any
// container is started.
func TestRun_InvalidNetworkConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{"both keys", "network_mode: host\nnetworks: [a]\n", "set either network_mode or networks, not both"},
		{"mapping-form networks", "networks:\n  a: {}\n", "per-network options are not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDir := setupNetworkRun(t, tt.yaml)
			fc := newFakeDocker(0, true)

			_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
			if err == nil || errors.Is(err, errSilent) {
				t.Fatalf("want a printable error, got %v; stderr=%q", err, stderr)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q missing %q", err, tt.wantSub)
			}
			if fc.Started || len(fc.ContainersInspected) > 0 || len(fc.NetworksInspected) > 0 {
				t.Error("container must not run and network preflight must not start on invalid config")
			}
		})
	}
}

// With docker.New() failed, the network preflight surfaces that error.
func TestNetworkPreflight_DockerNewErrStub(t *testing.T) {
	newErr := errors.New("docker client init failed")
	deps := dockerDeps{api: dockerNewErrStub{newErr}}
	for _, n := range []projectconfig.Network{
		{Mode: "container:proxy"},
		{Networks: []string{"a"}},
	} {
		if err := deps.networkPreflight(context.Background(), n); !errors.Is(err, newErr) {
			t.Errorf("%+v: err = %v, want wrapping %v", n, err, newErr)
		}
	}
}

// ── --join tests ──────────────────────────────────────────────────────────────

const emptyExcludeYAML = "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\n"

// joinRunFixture is a registered main project <parent>/app (cwd) and a sibling
// join project <parent>/lib, both with their own .makeslop.yaml.
type joinRunFixture struct {
	baseDir string
	app     string // resolved main root
	lib     string // resolved join root
}

func setupJoinRun(t *testing.T, appYAML, libYAML string) joinRunFixture {
	t.Helper()
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	parent := evalSymlinks(t, t.TempDir())
	app := filepath.Join(parent, "app")
	lib := filepath.Join(parent, "lib")
	mkdirAll(t, app)
	mkdirAll(t, lib)
	t.Chdir(app)
	initWithImage(t, baseDir)
	writeFile(t, filepath.Join(app, projectconfig.Filename), appYAML)
	writeFile(t, filepath.Join(lib, projectconfig.Filename), libYAML)
	return joinRunFixture{baseDir: baseDir, app: app, lib: lib}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRun_Join_DryRun_SeparatorsAndMounts_NoDaemonCalls(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	fc := newFakeDocker(0, false)
	fc.PingErr = errors.New("connection refused")

	stdout, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-n", "-j", "../lib")
	if err != nil {
		t.Fatalf("run -n -j failed: %v; stderr=%q", err, stderr)
	}
	for _, want := range []string{
		"`: '--- project: " + f.app + " ---'` \\\n",
		"`: '--- join: " + f.lib + " (rw) ---'` \\\n",
		"type=bind,source=" + f.lib + ",target=/workspace/lib",
		"type=bind,source=" + filepath.Join(f.lib, projectconfig.Filename) + ",target=/workspace/lib/.makeslop.yaml,readonly",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run stdout missing %q\nstdout:\n%s", want, stdout)
		}
	}
	if fc.DaemonChecked || fc.ImageChecked != "" || len(fc.ContainersInspected) > 0 || len(fc.NetworksInspected) > 0 {
		t.Errorf("dry-run with joins must make no daemon calls; daemon=%v image=%q", fc.DaemonChecked, fc.ImageChecked)
	}
}

func TestRun_Join_NoJoin_NoSeparators(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	stdout, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(newFakeDocker(0, false)), "run", "-n")
	if err != nil {
		t.Fatalf("run -n failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stdout, "`: '") || strings.Contains(stdout, "/workspace/lib") {
		t.Errorf("no --join: dry-run must have no separators or join mounts\nstdout:\n%s", stdout)
	}
}

func TestRun_Join_ReadOnlyVsReadWrite(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		wantRO      bool
		wantSandbox bool
	}{
		{name: "ro", flag: "../lib:ro", wantRO: true, wantSandbox: false},
		{name: "rw suffix", flag: "../lib:rw", wantRO: false, wantSandbox: true},
		{name: "no suffix", flag: "../lib", wantRO: false, wantSandbox: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
			if err := os.MkdirAll(filepath.Join(f.lib, ".git", "hooks"), 0o755); err != nil {
				t.Fatalf("mkdir .git/hooks: %v", err)
			}
			fc := newFakeDocker(0, true)
			_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", tc.flag)
			if err != nil {
				t.Fatalf("run -j failed: %v; stderr=%q", err, stderr)
			}
			bind, ok := findMount(fc.LastSpec.Mounts, "/workspace/lib")
			if !ok {
				t.Fatalf("join bind missing; mounts=%+v", fc.LastSpec.Mounts)
			}
			if bind.Host != f.lib || bind.ReadOnly != tc.wantRO {
				t.Errorf("join bind = %+v, want host %s readonly=%v", bind, f.lib, tc.wantRO)
			}
			_, hasCfg := findMount(fc.LastSpec.Mounts, "/workspace/lib/.makeslop.yaml")
			_, hasHooks := findMount(fc.LastSpec.Mounts, "/workspace/lib/.git/hooks")
			if hasCfg != tc.wantSandbox || hasHooks != tc.wantSandbox {
				t.Errorf("sandbox mounts: config=%v hooks=%v, want %v", hasCfg, hasHooks, tc.wantSandbox)
			}
			mode := "rw"
			if tc.wantRO {
				mode = "ro"
			}
			wantSections := []string{"project: " + f.app, "join: " + f.lib + " (" + mode + ")"}
			if got := sectionLabels(fc.LastSpec.Sections); !slices.Equal(got, wantSections) {
				t.Errorf("Section labels = %q, want %q", got, wantSections)
			}
			if want := mainWorkspacePath(t, f.baseDir, f.app); fc.LastSpec.Workdir != want || fc.LastSpec.Mounts[0].Container != want {
				t.Errorf("Workdir = %q, main bind = %q, want both %q", fc.LastSpec.Workdir, fc.LastSpec.Mounts[0].Container, want)
			}
		})
	}
}

func TestRun_Join_MaskingIsolation(t *testing.T) {
	appYAML := "exclude:\n  dirs: [mainprivate]\n  files: [mainsecret.txt]\n  scan:\n    patterns: [\"*.env\"]\n"
	libYAML := "exclude:\n  dirs: [private]\n  files: [secret.txt]\n  scan:\n    patterns: [\"*.key\"]\n"
	f := setupJoinRun(t, appYAML, libYAML)
	for _, p := range []string{
		filepath.Join(f.app, "a.env"), filepath.Join(f.app, "c.key"),
		filepath.Join(f.app, "secret.txt"), filepath.Join(f.app, "private", "x"),
		filepath.Join(f.app, "mainsecret.txt"), filepath.Join(f.app, "mainprivate", "x"),
		filepath.Join(f.lib, "b.env"), filepath.Join(f.lib, "c.key"),
		filepath.Join(f.lib, "secret.txt"), filepath.Join(f.lib, "private", "x"),
		filepath.Join(f.lib, "mainsecret.txt"), filepath.Join(f.lib, "mainprivate", "x"),
	} {
		writeFile(t, p, "S=1")
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
	if err != nil {
		t.Fatalf("run -j failed: %v; stderr=%q", err, stderr)
	}
	mainPath := fc.LastSpec.Workdir
	masked := func(container string) bool {
		m, ok := findMount(fc.LastSpec.Mounts, container)
		return ok && (m.Host == "/dev/null" || m.Type == "tmpfs")
	}
	want := map[string]bool{
		mainPath + "/a.env":             true,  // main pattern on main
		mainPath + "/c.key":             false, // join pattern never applies to main
		mainPath + "/secret.txt":        false, // join exclude.files never applies to main
		mainPath + "/private":           false, // join exclude.dirs never applies to main
		mainPath + "/mainsecret.txt":    true,
		mainPath + "/mainprivate":       true,
		"/workspace/lib/b.env":          false, // main pattern never applies to join
		"/workspace/lib/mainsecret.txt": false, // main exclude.files never applies to join
		"/workspace/lib/mainprivate":    false, // main exclude.dirs never applies to join
		"/workspace/lib/c.key":          true,
		"/workspace/lib/secret.txt":     true,
		"/workspace/lib/private":        true,
	}
	for target, w := range want {
		if got := masked(target); got != w {
			t.Errorf("masked(%s) = %v, want %v; mounts=%+v", target, got, w, fc.LastSpec.Mounts)
		}
	}
	if !strings.Contains(stderr, "makeslop: masked 1 secret file(s)\n") {
		t.Errorf("main masked line must be unchanged; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "makeslop: masked 1 secret file(s) in "+f.lib+"\n") {
		t.Errorf("join masked line missing; stderr=%q", stderr)
	}
}

func TestRun_Join_WarningsPrefixedAndQuiet(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	libYAML := "exclude:\n  dirs: []\n  files: [link.key]\n  scan:\n    patterns: [\"*.env\"]\n"
	f := setupJoinRun(t, emptyExcludeYAML, libYAML)
	writeFile(t, filepath.Join(f.lib, "real.env"), "S=1")
	writeFile(t, filepath.Join(f.lib, "real.key"), "K=1")
	if err := os.MkdirAll(filepath.Join(f.lib, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real.env", filepath.Join(f.lib, "sub", "link.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.key", filepath.Join(f.lib, "link.key")); err != nil {
		t.Fatal(err)
	}

	prefix := "makeslop: warning: join " + f.lib + ": "
	wantScanWarn := prefix + "symlink sub/link.env matches a secret pattern but is NOT masked"
	wantCfgWarn := prefix + `path "link.key" is a symlink and is NOT masked`
	maskedLine := "makeslop: masked 1 secret file(s) in " + f.lib

	_, stderr, err := runCmd(t, f.baseDir, "run", "-n", "-j", "../lib")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}
	for _, w := range []string{wantScanWarn, wantCfgWarn, maskedLine} {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr missing %q; got %q", w, stderr)
		}
	}

	_, stderrQ, err := runCmd(t, f.baseDir, "--quiet", "run", "-n", "-j", "../lib")
	if err != nil {
		t.Fatalf("quiet run failed: %v; stderr=%q", err, stderrQ)
	}
	for _, w := range []string{wantScanWarn, wantCfgWarn} {
		if !strings.Contains(stderrQ, w) {
			t.Errorf("--quiet must keep join warning %q; got %q", w, stderrQ)
		}
	}
	if strings.Contains(stderrQ, maskedLine) {
		t.Errorf("--quiet must hide the join masked line; got %q", stderrQ)
	}
}

func TestRun_Join_InvalidYAML_AbortsNamingJoin(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, "exclude:\n  dirs: [unclosed\n")
	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
	if err == nil {
		t.Fatalf("expected error for invalid join yaml; stderr=%q", stderr)
	}
	if !strings.HasPrefix(err.Error(), "join "+f.lib+": projectconfig: ") {
		t.Errorf("error must name the join: %v", err)
	}
	if fc.Started {
		t.Error("Run must not be called when a join config is invalid")
	}
}

func TestRun_Join_DaemonCheckedBeforeJoinYAML(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, "exclude:\n  dirs: [unclosed\n")
	fc := newFakeDocker(0, true)
	fc.PingErr = errors.New("connection refused")
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
	if !errors.Is(err, errSilent) {
		t.Fatalf("expected errSilent, got %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "is docker running") || strings.Contains(stderr, "projectconfig") {
		t.Errorf("daemon error must be reported before the join yaml is parsed; stderr=%q", stderr)
	}
	if fc.Started {
		t.Error("Run must not be called when the daemon is down")
	}
}

func TestRun_Join_ScanWalkError_Aborts(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	libYAML := "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: [\"*.env\"]\n"
	f := setupJoinRun(t, emptyExcludeYAML, libYAML)
	locked := filepath.Join(f.lib, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
	if err == nil {
		t.Fatalf("expected walk error; stderr=%q", stderr)
	}
	if !strings.HasPrefix(err.Error(), "join "+f.lib+": ") {
		t.Errorf("walk error must name the join: %v", err)
	}
	if fc.Started {
		t.Error("Run must not be called after a join scan walk error")
	}
}

func TestRun_Join_EnvAndNetworkIgnored(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"static env", "environments:\n  static:\n    JOIN_VAR: x\n"},
		{"host env", "environments:\n  host: [JOIN_HOST_VAR]\n"},
		{"network_mode", "network_mode: host\n"},
		{"networks", "networks: [joinnet]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JOIN_HOST_VAR", "leak")
			f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML+tc.yaml)
			fc := newFakeDocker(0, true)
			_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
			if err != nil {
				t.Fatalf("run -j failed: %v; stderr=%q", err, stderr)
			}
			if fc.LastSpec.Env != nil || fc.LastSpec.NetworkMode != "" || fc.LastSpec.Networks != nil {
				t.Errorf("join env/network must be ignored; Env=%v NetworkMode=%q Networks=%v",
					fc.LastSpec.Env, fc.LastSpec.NetworkMode, fc.LastSpec.Networks)
			}
			if len(fc.NetworksInspected) > 0 || len(fc.ContainersInspected) > 0 {
				t.Errorf("join network settings must not be preflighted; networks=%v containers=%v",
					fc.NetworksInspected, fc.ContainersInspected)
			}
			line := "makeslop: join " + f.lib + ": environments/network settings ignored\n"
			if n := strings.Count(stderr, line); n != 1 {
				t.Errorf("want exactly one ignored line %q, got %d; stderr=%q", line, n, stderr)
			}

			_, stderrQ, err := runCmdWithDeps(t, f.baseDir, depsFrom(newFakeDocker(0, true)), "--quiet", "run", "-j", "../lib")
			if err != nil {
				t.Fatalf("quiet run -j failed: %v; stderr=%q", err, stderrQ)
			}
			if strings.Contains(stderrQ, "ignored") {
				t.Errorf("--quiet must hide the ignored line; stderr=%q", stderrQ)
			}
		})
	}
}

func TestRun_Join_NoIgnoredLineForPlainJoin(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(newFakeDocker(0, true)), "run", "-j", "../lib")
	if err != nil {
		t.Fatalf("run -j failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "ignored") {
		t.Errorf("plain join must not print the ignored line; stderr=%q", stderr)
	}
}

func TestRun_Join_ResolveErrorBeforeDaemon(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	fc := newFakeDocker(0, true)
	_, _, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../missing")
	if err == nil || !strings.Contains(err.Error(), `--join "../missing"`) {
		t.Fatalf("expected --join resolve error, got %v", err)
	}
	if fc.DaemonChecked || fc.Started {
		t.Errorf("join path errors must fire before the daemon preflight")
	}
}

func sectionLabels(secs []docker.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Label)
	}
	return out
}

// mainWorkspacePath is the container path of the main project registered at
// root, derived from the workspace registry (its cache dir name).
func mainWorkspacePath(t *testing.T, baseDir, root string) string {
	t.Helper()
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	_, cacheDir, err := workspace.New(baseDir).Lookup(s, root)
	if err != nil {
		t.Fatalf("lookup %s: %v", root, err)
	}
	return "/workspace/" + filepath.Base(cacheDir)
}

func TestRun_Join_DataDirRejected(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	inData := filepath.Join(evalSymlinks(t, f.baseDir), "proj")
	writeFile(t, filepath.Join(inData, projectconfig.Filename), emptyExcludeYAML)

	fc := newFakeDocker(0, true)
	_, _, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", inData)
	if err == nil || !strings.Contains(err.Error(), "overlaps the makeslop data dir") {
		t.Fatalf("expected data dir overlap error, got %v", err)
	}
	if fc.DaemonChecked || fc.Started {
		t.Error("data dir overlap must fire before the daemon preflight")
	}
}

func TestRun_Join_RelativeToCwdSubdir(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	sub := filepath.Join(f.app, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../../lib")
	if err != nil {
		t.Fatalf("run -j ../../lib from app/sub failed: %v; stderr=%q", err, stderr)
	}
	if bind, ok := findMount(fc.LastSpec.Mounts, "/workspace/lib"); !ok || bind.Host != f.lib {
		t.Errorf("join bind = %+v (found=%v), want host %s", bind, ok, f.lib)
	}

	fc = newFakeDocker(0, true)
	_, _, err = runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "..")
	if want := `--join "..": is the current project`; err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	if fc.DaemonChecked {
		t.Error("resolve error must fire before the daemon preflight")
	}
}

// The join's config is required at load time too: one deleted after
// resolveJoins (e.g. during the daemon preflight) must fail the run rather
// than mount the join with no masking.
func TestRun_Join_ConfigDeletedAfterResolve(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, "exclude:\n  files: [secret.txt]\n")
	fc := newFakeDocker(0, true)
	fc.OnCheckDaemon = func() {
		if err := os.Remove(filepath.Join(f.lib, projectconfig.Filename)); err != nil {
			t.Errorf("remove join config: %v", err)
		}
	}
	_, _, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../lib")
	want := "join " + f.lib + ": not a makeslop project (no .makeslop.yaml)"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	if !fc.DaemonChecked || fc.Started {
		t.Errorf("want daemon checked and no Run; daemon=%v started=%v", fc.DaemonChecked, fc.Started)
	}
}

func TestRun_Join_OutOfHome(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	home := filepath.Dir(f.app) // app is inside $HOME, the new join is not
	t.Setenv("HOME", home)
	outside := filepath.Join(evalSymlinks(t, t.TempDir()), "ext")
	writeFile(t, filepath.Join(outside, projectconfig.Filename), emptyExcludeYAML)

	fc := newFakeDocker(0, true)
	_, _, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", outside)
	want := `--join "` + outside + `": outside ` + home + ` — pass --out-of-home to override`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	if fc.DaemonChecked || fc.Started {
		t.Error("home guard must fire before the daemon preflight")
	}

	fc = newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "--out-of-home", "-j", outside)
	if err != nil {
		t.Fatalf("run --out-of-home -j failed: %v; stderr=%q", err, stderr)
	}
	if bind, ok := findMount(fc.LastSpec.Mounts, "/workspace/ext"); !ok || bind.Host != outside {
		t.Errorf("join bind = %+v (found=%v), want host %s", bind, ok, outside)
	}
}

func TestRun_Join_TwoJoins_OrderAndSecondLoadError(t *testing.T) {
	f := setupJoinRun(t, emptyExcludeYAML, emptyExcludeYAML)
	tools := filepath.Join(filepath.Dir(f.app), "tools")
	writeFile(t, filepath.Join(tools, projectconfig.Filename), emptyExcludeYAML)

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../tools:ro", "-j", "../lib")
	if err != nil {
		t.Fatalf("run with two joins failed: %v; stderr=%q", err, stderr)
	}
	wantLabels := []string{"project: " + f.app, "join: " + tools + " (ro)", "join: " + f.lib + " (rw)"}
	if got := sectionLabels(fc.LastSpec.Sections); !slices.Equal(got, wantLabels) {
		t.Errorf("Section labels = %q, want %q", got, wantLabels)
	}
	secs := fc.LastSpec.Sections
	if len(secs) == 3 {
		if m := fc.LastSpec.Mounts[secs[1].Start]; m.Container != "/workspace/tools" || !m.ReadOnly {
			t.Errorf("section 1 starts at %+v, want ro /workspace/tools bind", m)
		}
		if m := fc.LastSpec.Mounts[secs[2].Start]; m.Container != "/workspace/lib" || m.ReadOnly {
			t.Errorf("section 2 starts at %+v, want rw /workspace/lib bind", m)
		}
	}

	// Second join broken: the first loads fine, the error names the second.
	writeFile(t, filepath.Join(f.lib, projectconfig.Filename), "exclude:\n  dirs: [unclosed\n")
	fc = newFakeDocker(0, true)
	_, _, err = runCmdWithDeps(t, f.baseDir, depsFrom(fc), "run", "-j", "../tools:ro", "-j", "../lib")
	if err == nil || !strings.HasPrefix(err.Error(), "join "+f.lib+": projectconfig: ") {
		t.Errorf("error must name the second join: %v", err)
	}
	if fc.Started {
		t.Error("Run must not be called when a later join fails to load")
	}
}

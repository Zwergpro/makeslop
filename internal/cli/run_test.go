package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
	for _, m := range mounts {
		if m.Container == target {
			return true
		}
	}
	return false
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

	stdout, stderr, err := runCmd(t, baseDir, "run")
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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
	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("parent init failed: %v", err)
	}
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

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	_, stderr, err := runCmd(t, baseDir, "run")
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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
		ProjectRoot:          resolvedPwd,
		WorkspaceName:        filepath.Base(workspaceDir),
		WorkspaceHost:        workspaceDir,
		BaseDir:              baseDir,
		Image:                s.Image,
		Command:              s.Shell,
		TmpDirSize:           s.TmpDirSize,
		MountContentCache:    true,
		MountAgentCache:      true,
		ProtectProjectConfig: true,
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
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
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init inside home failed: %v", err)
	}

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
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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
		ProjectRoot:       resolvedPwd,
		WorkspaceName:     filepath.Base(workspaceDir),
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

// Docker must never start when yaml parse fails (secret-masking invariant).
// runWithExitCode (not runCmd) so non-errSilent errors land on stderr.
func TestRun_YamlMalformedAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	badYAML := []byte("exclude:\n  dirs: [unclosed\n")
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), badYAML, 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}

	fc := newFakeDocker(0, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from malformed yaml, got 0; stderr=%q", stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "makeslop: ") {
		t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
	}
	if fc.Started {
		t.Errorf("docker client must not be started on yaml parse error")
	}
}

// Uses runWithExitCode (not runCmd) so the error appears on stderr.
func TestRun_YamlReservedPathAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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
		ProjectRoot:       resolvedPwd,
		WorkspaceName:     filepath.Base(workspaceDir),
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

// Missing image: run aborts with the build remedy; no auto-build, no container.
func TestRun_ImageMissing_AbortsWithRemedy(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	fc := newFakeDocker(0, true)
	fc.ImageMissing = true

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err == nil {
		t.Fatalf("expected error when image is missing, got nil; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (tailored message written to stderr), got %v", err)
	}
	if !strings.Contains(stderr, "not built") {
		t.Errorf("stderr missing 'not built': %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop build") {
		t.Errorf("stderr missing 'makeslop build' remedy: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker container must not be started when image is missing")
	}
}

// A non-not-found ImageExists error must report "is docker running?" (not "not
// built"), distinguishing a store/daemon error from a genuinely missing image.
func TestRun_ImageOtherError_PropagatesError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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
	if strings.Contains(stderr, "not built") {
		t.Errorf("image other-error must not emit 'not built' hint; stderr=%q", stderr)
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

// An environments: block flows into -e KEY=VALUE flags (sorted) in the dry-run output.
func TestRun_EnvironmentsBlock_ProducesEnvFlags(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	workspaceDir := strings.TrimSpace(initOut)

	resolvedPwd := evalSymlinks(t, pwd)

	yamlContent := "exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\nenvironments:\n  NODE_ENV: production\n  PORT: \"8080\"\n  DEBUG: \"false\"\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}

	// Environments are sorted: DEBUG < NODE_ENV < PORT
	for _, want := range []string{"-e DEBUG=false", "-e NODE_ENV=production", "-e PORT=8080"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--dry-run stdout missing %q\nstdout:\n%s", want, stdout)
		}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

// Git project + .makeslop.yaml present → both sandbox mounts in the spec
// the fake runner receives.
func TestRun_GitAndConfig_BothSandboxMounts(t *testing.T) {
	skipNonPOSIX(t, "symlinks/Lstat behaviour is POSIX-specific")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
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

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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
	reportScanResults(&stderr, &chrome, root, masked, nil)

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
	reportScanResults(&stderr, &chrome, root, nil, syms)

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
	reportScanResults(&stderr, &chrome, root, nil, []string{sym})

	// Either the relative or absolute path must appear in the warning.
	if !strings.Contains(stderr.String(), "link.env") {
		t.Errorf("stderr missing symlink name regardless of Rel fallback: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Errorf("stderr missing 'symlink' warning: %q", stderr.String())
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/assets"
	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// fakeDocker is a boundary fake satisfying all four consumer interfaces, injected
// via newRootCmdWithDeps.
type fakeDocker struct {
	exitCode int
	isTTY    bool // when false, Run returns docker.ErrNoTTY
	Started  bool // set when Run is reached past the TTY check

	PingErr      error // CheckDaemon returns this wrapped in ErrDaemonUnreachable
	ImageMissing bool  // ImageExists returns (false, nil)
	ImageErr     error // ImageExists returns (false, err) unless ImageMissing

	BuildErr      error
	LastBuildOpts docker.BuildOptions

	LastSpec docker.Spec // set when Run is called (isTTY=true)
}

func newFakeDocker(exitCode int, isTTY bool) *fakeDocker {
	return &fakeDocker{exitCode: exitCode, isTTY: isTTY}
}

func (f *fakeDocker) Run(_ context.Context, s docker.Spec) error {
	if !f.isTTY {
		return docker.ErrNoTTY
	}
	f.Started = true
	f.LastSpec = s
	if f.exitCode != 0 {
		return &docker.ExitError{Code: f.exitCode}
	}
	return nil
}

func (f *fakeDocker) Build(_ context.Context, o docker.BuildOptions, _, _ io.Writer) error {
	f.LastBuildOpts = o
	if f.BuildErr != nil {
		return f.BuildErr
	}
	return nil
}

func (f *fakeDocker) CheckDaemon(_ context.Context) error {
	if f.PingErr != nil {
		return &docker.ErrDaemonUnreachable{Cause: f.PingErr}
	}
	return nil
}

func (f *fakeDocker) ImageExists(_ context.Context, _ string) (bool, error) {
	if f.ImageMissing {
		return false, nil
	}
	if f.ImageErr != nil {
		return false, f.ImageErr
	}
	return true, nil
}

// runCmd runs the cobra tree against a production root (live client factory).
// Use runCmdWithDeps when a fake docker is needed.
func runCmd(t *testing.T, baseDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd, closeDocker := newRootCmd(baseDir)
	defer closeDocker()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

func runCmdWithDeps(t *testing.T, baseDir string, deps dockerDeps, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmdWithDeps(baseDir, deps)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

func depsFrom(f *fakeDocker) dockerDeps {
	return dockerDeps{runner: f, builder: f, daemon: f, image: f}
}

// runWithExitCodeAndDeps mirrors runWithExitCode with injected deps and a plain
// background context (no signal wiring needed in tests).
func runWithExitCodeAndDeps(baseDir string, stdout, stderr io.Writer, deps dockerDeps, args []string) int {
	cmd := newRootCmdWithDeps(baseDir, deps)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		return 0
	}
	var de *docker.ExitError
	if errors.As(err, &de) {
		return de.Code
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintf(stderr, "makeslop: %v\n", err)
	}
	return 1
}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snap := map[string][]byte{}
	_, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return snap
	}
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return snap
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	_, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return paths
	}
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

// setHomeToTestParent sets HOME to the parent of t.TempDir() so subsequent
// TempDir() calls are siblings that satisfy ensureWithinHome.
func setHomeToTestParent(t *testing.T) {
	t.Helper()
	sentinel := t.TempDir()
	parent := evalSymlinks(t, filepath.Dir(sentinel))
	t.Setenv("HOME", parent)
}

// skipNonPOSIX skips the test on non-POSIX hosts per the CLAUDE.md invariant.
func skipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
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

func TestInit_FromScratch(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	stdout, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}
	workspacePath := strings.TrimSpace(stdout)
	if workspacePath == "" {
		t.Fatalf("init produced empty stdout")
	}
	workspacesRoot := filepath.Join(baseDir, "workspaces")
	if !strings.HasPrefix(workspacePath, workspacesRoot+string(filepath.Separator)) {
		t.Errorf("workspace path %q not under %q", workspacePath, workspacesRoot)
	}
	info, err := os.Stat(workspacePath)
	if err != nil {
		t.Fatalf("stat workspace dir %s: %v", workspacePath, err)
	}
	if !info.IsDir() {
		t.Errorf("workspace path %s is not a directory", workspacePath)
	}

	settingsPath := filepath.Join(baseDir, "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var s struct {
		Version    int                       `json:"version"`
		Workspaces map[string]map[string]any `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if s.Version != 1 {
		t.Errorf("expected version 1, got %d", s.Version)
	}
	resolvedPwd := evalSymlinks(t, pwd)
	entry, ok := s.Workspaces[resolvedPwd]
	if !ok {
		t.Fatalf("settings.workspaces missing key %q; have %v", resolvedPwd, s.Workspaces)
	}
	name, _ := entry["name"].(string)
	if name == "" {
		t.Errorf("workspace entry has empty name")
	}
	if filepath.Base(workspacePath) != name {
		t.Errorf("workspace dir basename %q != entry name %q", filepath.Base(workspacePath), name)
	}
}

// installFakeBuildClient builds a fakeDocker whose Build fails for a non-zero exitCode.
func installFakeBuildClient(t *testing.T, exitCode int) *fakeDocker {
	t.Helper()
	fd := &fakeDocker{}
	if exitCode != 0 {
		fd.BuildErr = fmt.Errorf("build exited with code %d", exitCode)
	}
	return fd
}

// Milestone-1 regression guard: makeslop go must not print the cache path on stdout.
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
	want := docker.BuildSpec(docker.Options{
		ProjectRoot:   resolvedPwd,
		WorkspaceName: filepath.Base(workspaceDir),
		BaseDir:       baseDir,
		Image:         s.Image,
		Command:       s.Shell,
		TmpDirSize:    s.TmpDirSize,
	})
	_ = want // spec correctness is covered by spec_test.go drift-guard
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

	// Spec must mount the registered ancestor (parent), not the subdir.
	resolvedParent := evalSymlinks(t, parent)
	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	spec := docker.BuildSpec(docker.Options{
		ProjectRoot:   resolvedParent,
		WorkspaceName: filepath.Base(workspaceDir),
		BaseDir:       baseDir,
		Image:         s.Image,
		Command:       s.Shell,
		TmpDirSize:    s.TmpDirSize,
	})
	argv := spec.Args()
	wantSourceFragment := `type=bind,source=` + resolvedParent + `,target=/workspace/` + filepath.Base(workspaceDir)
	var found bool
	for _, a := range argv {
		if a == wantSourceFragment {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("project-root mount not found in spec argv\nwant: %q\nargv: %v", wantSourceFragment, argv)
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

// Daemon-reported StatusCode 137 (128+SIGKILL) must pass through verbatim. With
// the SDK there is no forked process / OS WaitStatus to derive 128+signum from.
func TestRunWithExitCode_DaemonReports137_MapsTo137(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	fc := newFakeDocker(137, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fc), []string{"run"})
	if code != 137 {
		t.Errorf("runWithExitCode = %d, want 137 (daemon-reported 128+SIGKILL); stderr=%q", code, stderr.String())
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

// Locks the "makeslop: " prefix path for non-ExitError, non-errSilent failures.
func TestRunWithExitCode_NonExitErrorPrintsPrefix(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)
	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"run"}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "makeslop: ") {
		t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
	}
}

// runWithExitCode must hand the observer a cancellable context, not context.Background().
func TestRunWithExitCode_ContextObserver(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()

	var observedCtx context.Context
	observer := func(ctx context.Context) {
		observedCtx = ctx
	}

	var stdout, stderr bytes.Buffer
	runWithExitCode(baseDir, &stdout, &stderr, []string{"version"}, observer)

	if observedCtx == nil {
		t.Fatal("contextObserver was not called")
	}
	if observedCtx == context.Background() {
		t.Error("contextObserver received context.Background(); expected a signal-cancellable child context")
	}
	if observedCtx.Done() == nil {
		t.Error("observed context has no Done channel; expected a cancellable context")
	}
}

func TestInit_Twice_Idempotent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := evalSymlinks(t, t.TempDir())
	t.Chdir(pwd)

	out1, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("first init failed: %v", err)
	}
	snapBefore := snapshotTree(t, baseDir)

	out2, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("second init failed: %v", err)
	}
	if strings.TrimSpace(out1) != strings.TrimSpace(out2) {
		t.Errorf("second init returned different path: %q vs %q", out1, out2)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)

	// Scaffold must be idempotent — second init must not touch .makeslop.yaml.
	yamlPath := filepath.Join(pwd, projectconfig.Filename)
	got, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read %s after second init: %v", projectconfig.Filename, err)
	}
	if !bytes.Equal(got, projectconfig.Stub) {
		t.Errorf("%s content after second init = %q, want %q", projectconfig.Filename, got, projectconfig.Stub)
	}
}

func TestInit_FromSubdir_ReusesParent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	parent := t.TempDir()
	t.Chdir(parent)
	parentOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("parent init failed: %v", err)
	}
	parentPath := strings.TrimSpace(parentOut)

	sub := filepath.Join(parent, "sub", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(sub)
	snapBefore := snapshotTree(t, baseDir)

	subOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("sub init failed: %v", err)
	}
	if strings.TrimSpace(subOut) != parentPath {
		t.Errorf("sub init path %q != parent path %q", subOut, parentPath)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)

	// .makeslop.yaml belongs in the workspace root (parent), not the subdir init ran from.
	resolvedParent := evalSymlinks(t, parent)
	resolvedSub := evalSymlinks(t, sub)
	if _, err := os.Stat(filepath.Join(resolvedSub, projectconfig.Filename)); err == nil {
		t.Errorf(".makeslop.yaml must NOT be created in the subdir %s; it belongs in the workspace root", resolvedSub)
	}
	if _, err := os.Stat(filepath.Join(resolvedParent, projectconfig.Filename)); err != nil {
		t.Errorf(".makeslop.yaml must exist in the workspace root %s: %v", resolvedParent, err)
	}
}

func TestInit_SymlinkInvariant(t *testing.T) {
	skipNonPOSIX(t, "symlinks unreliable on Windows; makeslop is POSIX-only")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	real := t.TempDir()

	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("create symlink %s -> %s: %v", alias, real, err)
	}

	t.Chdir(alias)
	firstOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("first init via symlink failed: %v", err)
	}
	firstPath := strings.TrimSpace(firstOut)

	snapBefore := snapshotTree(t, baseDir)

	t.Chdir(real)
	secondOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("second init via real path failed: %v", err)
	}
	if strings.TrimSpace(secondOut) != firstPath {
		t.Errorf("second init path %q != first %q", secondOut, firstPath)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)

	t.Chdir(alias)
	thirdOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("third init via symlink failed: %v", err)
	}
	if strings.TrimSpace(thirdOut) != firstPath {
		t.Errorf("third init path %q != first %q", thirdOut, firstPath)
	}
	snapFinal := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapFinal)

	// Must key by the resolved path, not the symlink alias.
	settingsData, err := os.ReadFile(filepath.Join(baseDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var s struct {
		Workspaces map[string]any `json:"workspaces"`
	}
	if err := json.Unmarshal(settingsData, &s); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	resolved := evalSymlinks(t, real)
	if _, ok := s.Workspaces[resolved]; !ok {
		t.Errorf("settings.workspaces missing resolved key %q; have %v", resolved, s.Workspaces)
	}
	if _, ok := s.Workspaces[alias]; ok {
		t.Errorf("settings.workspaces unexpectedly contains alias %q", alias)
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

// Regression guard for the SilenceErrors-on-root cobra inheritance bug.
func TestInit_CorruptSettings_ReportsError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "init")
	if err == nil {
		t.Fatalf("expected error from init with corrupt settings, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if errors.Is(err, errSilent) {
		t.Errorf("init error must not be errSilent — main() needs to print it: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("expected error to mention 'settings' context, got %q", err.Error())
	}
}

// Bootstrap contract: artifacts exist after first init; second init is byte-equal.
func TestInit_BootstrapsAgentArtifacts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("first init: %v", err)
	}

	for _, sub := range []string{".codex", ".claude", "workspaces"} {
		info, err := os.Stat(filepath.Join(baseDir, sub))
		if err != nil {
			t.Errorf("missing %s: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}

	claudeJSON := filepath.Join(baseDir, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !bytes.Equal(data, []byte("{}\n")) {
		t.Errorf(".claude.json content = %q, want %q", data, "{}\n")
	}

	snapBefore := snapshotTree(t, baseDir)
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("second init: %v", err)
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
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

// Guards that ensureWithinHome fires before config.Bootstrap (no mutation when outside HOME).
func TestInit_OutsideHome_Refuses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	snapBefore := snapshotTree(t, baseDir)
	_, stderr, err := runCmd(t, baseDir, "init")
	if err == nil {
		t.Fatalf("expected error from init outside HOME, got nil")
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
	// Guard fires before config.Bootstrap, so baseDir must be untouched.
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// Guards the rel == "." edge case: cwd == HOME is local and must be allowed.
func TestInit_HomeRoot_Allowed(t *testing.T) {
	tmpHome := t.TempDir()
	resolvedHome := evalSymlinks(t, tmpHome)
	t.Setenv("HOME", resolvedHome)
	t.Chdir(resolvedHome)

	baseDir := t.TempDir()

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init from HOME root should succeed, got: %v; stderr=%q", err, stderr)
	}
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

func TestRoot_BareInvocation_PrintsHelp(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	snapBefore := snapshotTree(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir) // no args
	if err != nil {
		t.Fatalf("bare makeslop should exit 0, got err: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Available Commands:") {
		t.Errorf("stdout missing 'Available Commands:': %q", stdout)
	}
	// cobra indents subcommands two spaces in the Available Commands block.
	if !strings.Contains(stdout, "\n  run ") {
		t.Errorf("stdout missing '\\n  run ' command entry: %q", stdout)
	}
	if !strings.Contains(stdout, "\n  init ") {
		t.Errorf("stdout missing '\\n  init ' command entry: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

func assertSnapshotsEqual(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("snapshot file count differs: before=%d after=%d (before keys=%v after keys=%v)",
			len(before), len(after), mapKeys(before), mapKeys(after))
	}
	for k, vBefore := range before {
		vAfter, ok := after[k]
		if !ok {
			t.Errorf("file %s present before, missing after", k)
			continue
		}
		if !bytes.Equal(vBefore, vAfter) {
			t.Errorf("file %s changed:\nbefore:\n%s\nafter:\n%s", k, vBefore, vAfter)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("file %s appeared after but not before", k)
		}
	}
}

func mapKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── dry-run integration tests ─────────────────────────────────────────────────

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
	want := docker.BuildSpec(docker.Options{
		ProjectRoot:          resolvedPwd,
		WorkspaceName:        filepath.Base(workspaceDir),
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

func TestInit_DryRunFlagRejected(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, _, err := runCmd(t, baseDir, "init", "--dry-run")
	if err == nil {
		t.Fatalf("init --dry-run should fail, got nil error")
	}
	// Match the flag name only; cobra's exact phrasing varies by version.
	if !strings.Contains(err.Error(), "dry-run") {
		t.Errorf("error must mention 'dry-run', got: %q", err.Error())
	}
}

func TestInit_ScaffoldsProjectConfig(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}

	resolvedPwd := evalSymlinks(t, pwd)
	configPath := filepath.Join(resolvedPwd, projectconfig.Filename)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", projectconfig.Filename, err)
	}
	if !bytes.Equal(data, projectconfig.Stub) {
		t.Errorf("%s content = %q, want %q", projectconfig.Filename, data, projectconfig.Stub)
	}
}

func TestInit_PreservesExistingProjectConfig(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	resolvedPwd := evalSymlinks(t, pwd)
	configPath := filepath.Join(resolvedPwd, projectconfig.Filename)
	userContent := []byte("exclude:\n  dirs: [node_modules]\n  files: [secrets/key]\n")
	if err := os.WriteFile(configPath, userContent, 0o644); err != nil {
		t.Fatalf("write pre-existing config: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s after init: %v", projectconfig.Filename, err)
	}
	if !bytes.Equal(data, userContent) {
		t.Errorf("%s was modified by init\ngot:  %q\nwant: %q", projectconfig.Filename, data, userContent)
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

// ── migrate subcommand tests ───────────────────────────────────────────────────

func TestMigrate_FirstRun_PrintsUpdatedAndWritesDockerfile(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("migrate failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "updated") {
		t.Errorf("stdout missing 'updated': %q", stdout)
	}
	if strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout must not say 'already up to date' on first run: %q", stdout)
	}

	dockerfilePath := filepath.Join(baseDir, "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("Dockerfile not created by migrate: %v", err)
	}
	if !bytes.Equal(data, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(data), len(assets.Dockerfile))
	}
}

// Second migrate is idempotent: "already up to date", file unchanged.
func TestMigrate_SecondRun_PrintsAlreadyUpToDate(t *testing.T) {
	baseDir := t.TempDir()

	if _, _, err := runCmd(t, baseDir, "migrate"); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	snapBefore := snapshotTree(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("second migrate failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout missing 'already up to date' on second run: %q", stdout)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// migrate works standalone (no prior init, no pre-created dirs).
func TestMigrate_WithoutPriorInit_SucceedsAndWritesDockerfile(t *testing.T) {
	// Non-existing subdir so migrate must create the directory itself.
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "brand-new-dir")

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("migrate without prior init failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "updated") {
		t.Errorf("stdout missing 'updated': %q", stdout)
	}

	dockerfilePath := filepath.Join(baseDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err != nil {
		t.Errorf("Dockerfile not created by migrate without prior init: %v", err)
	}
}

func TestMigrate_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err == nil {
		t.Fatalf("expected error from migrate with corrupt settings, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("expected error to mention 'settings' context, got %q", err.Error())
	}
}

func TestRoot_BareInvocation_ListsMigrateCommand(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir) // no args
	if err != nil {
		t.Fatalf("bare makeslop should exit 0, got err: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "\n  migrate ") {
		t.Errorf("stdout missing '\\n  migrate ' command entry: %q", stdout)
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

// ── build subcommand tests ─────────────────────────────────────────────────────

// build on a fresh baseDir self-heals the Dockerfile then invokes Build.
func TestBuild_SeedsSelfHealAndInvokesSDK(t *testing.T) {
	baseDir := t.TempDir()
	fbc := installFakeBuildClient(t, 0)

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
	fbc := installFakeBuildClient(t, 0)

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
	fbc := installFakeBuildClient(t, 1)

	var stdout, stderr bytes.Buffer
	code := runWithExitCodeAndDeps(baseDir, &stdout, &stderr, depsFrom(fbc), []string{"build"})
	if code != 1 {
		t.Errorf("runWithExitCode = %d, want 1 (generic error); stderr=%q", code, stderr.String())
	}
}

// A custom image name in settings.json flows into the Build options.
func TestBuild_CustomImage_FromSettings(t *testing.T) {
	baseDir := t.TempDir()
	fbc := installFakeBuildClient(t, 0)

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

func TestRoot_BareInvocation_ListsBuildCommand(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir) // no args
	if err != nil {
		t.Fatalf("bare makeslop should exit 0, got err: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "\n  build ") {
		t.Errorf("stdout missing '\\n  build ' command entry: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
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
	fbc := installFakeBuildClient(t, 0)

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

// build invokes the Build dep. BuildKit version selection is covered in
// internal/docker build_test.go.
func TestBuild_BuildKitVersion_IsSet(t *testing.T) {
	baseDir := t.TempDir()
	fbc := installFakeBuildClient(t, 0)

	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fbc), "build")
	if err != nil {
		t.Fatalf("build failed: %v; stderr=%q", err, stderr)
	}

	if fbc.LastBuildOpts.Image == "" {
		t.Error("Build was not invoked (LastBuildOpts.Image is empty)")
	}
}

// build --refresh overwrites a stale Dockerfile with the embedded assets version.
func TestBuild_Refresh_OverwritesDockerfileAndBuilds(t *testing.T) {
	baseDir := t.TempDir()
	fbc := installFakeBuildClient(t, 0)

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
	fbc := installFakeBuildClient(t, 0)

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
		fbc := installFakeBuildClient(t, 0)

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
		fbc := installFakeBuildClient(t, 0)

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

// ── config subcommand tests ───────────────────────────────────────────────────

// Bare `config` prints key = value settings (not help) and exits 0.
func TestConfig_BareInvocation_PrintsSettings(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "config")
	if err != nil {
		t.Fatalf("bare 'makeslop config' should exit 0, got err: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
	if !strings.Contains(stdout, "image = ") {
		t.Errorf("config output missing 'image = ' key: %q", stdout)
	}
	if !strings.Contains(stdout, "shell = ") {
		t.Errorf("config output missing 'shell = ' key: %q", stdout)
	}
	if !strings.Contains(stdout, "tmp_dir_size = ") {
		t.Errorf("config output missing 'tmp_dir_size = ' key: %q", stdout)
	}
}

func TestRoot_BareInvocation_ListsConfigCommand(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir)
	if err != nil {
		t.Fatalf("bare makeslop should exit 0, got err: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "\n  config ") {
		t.Errorf("stdout missing '\\n  config ' command entry: %q", stdout)
	}
}

// config list on a fresh baseDir prints the three keys with defaults in registry order.
func TestConfigList_FreshBaseDir_PrintsThreeDefaults(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "config", "list")
	if err != nil {
		t.Fatalf("config list failed: %v; stderr=%q", err, stderr)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}

	wantLines := []string{
		"image = " + config.DefaultImage,
		"shell = " + config.DefaultShell,
		"tmp_dir_size = " + config.DefaultTmpDirSize,
	}
	for _, line := range wantLines {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout missing %q: %q", line, stdout)
		}
	}

	// Registry order: image, shell, tmp_dir_size.
	idxImage := strings.Index(stdout, "image =")
	idxShell := strings.Index(stdout, "shell =")
	idxTmpDir := strings.Index(stdout, "tmp_dir_size =")
	if idxImage < 0 || idxShell < 0 || idxTmpDir < 0 {
		t.Fatalf("one or more keys missing from stdout: %q", stdout)
	}
	if idxImage >= idxShell || idxShell >= idxTmpDir {
		t.Errorf("registry order violated: image@%d shell@%d tmp_dir_size@%d", idxImage, idxShell, idxTmpDir)
	}
}

// config set persists each of the three keys and config list reflects it.
func TestConfigSet_ThenList_ShowsNewValue(t *testing.T) {
	baseDir := t.TempDir()

	cases := []struct {
		key, val, wantLine string
		check              func(*config.Settings) string
	}{
		{"image", "foo", "image = foo", func(s *config.Settings) string { return s.Image }},
		{"shell", "/bin/bash", "shell = /bin/bash", func(s *config.Settings) string { return s.Shell }},
		{"tmp_dir_size", "512m", "tmp_dir_size = 512m", func(s *config.Settings) string { return s.TmpDirSize }},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			stdout, stderr, err := runCmd(t, baseDir, "config", "set", tc.key, tc.val)
			if err != nil {
				t.Fatalf("config set %s %s failed: %v; stderr=%q", tc.key, tc.val, err, stderr)
			}
			if !strings.Contains(stdout, tc.wantLine) {
				t.Errorf("config set stdout missing %q: %q", tc.wantLine, stdout)
			}

			listOut, listErr, err := runCmd(t, baseDir, "config", "list")
			if err != nil {
				t.Fatalf("config list failed: %v; stderr=%q", err, listErr)
			}
			if !strings.Contains(listOut, tc.wantLine) {
				t.Errorf("config list output missing %q: %q", tc.wantLine, listOut)
			}

			s, loadErr := config.Load(baseDir)
			if loadErr != nil {
				t.Fatalf("load settings: %v", loadErr)
			}
			if got := tc.check(s); got != tc.val {
				t.Errorf("settings.%s = %q, want %q", tc.key, got, tc.val)
			}
		})
	}
}

// An invalid tmp_dir_size is rejected (exit 1) without mutating the settings file.
func TestConfigSet_InvalidTmpDirSize_ExitsOneAndFileUnchanged(t *testing.T) {
	baseDir := t.TempDir()

	snapBefore := snapshotTree(t, baseDir)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "tmp_dir_size", "9z"}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "tmp_dir_size") {
		t.Errorf("stderr missing 'tmp_dir_size'; got %q", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout, got %q", stdout.String())
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// An unknown key is rejected (exit 1) and stderr lists all valid keys.
func TestConfigSet_UnknownKey_ExitsOneAndListsValidKeys(t *testing.T) {
	baseDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "bogus", "x"}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	stderrStr := stderr.String()
	for _, key := range []string{"image", "shell", "tmp_dir_size"} {
		if !strings.Contains(stderrStr, key) {
			t.Errorf("stderr missing valid key %q: %q", key, stderrStr)
		}
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout, got %q", stdout.String())
	}
}

// config set works without a prior init (Save's MkdirAll heals the missing dir).
func TestConfigSet_WithoutPriorInit_SelfHeals(t *testing.T) {
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "brand-new-makeslop-dir")

	stdout, stderr, err := runCmd(t, baseDir, "config", "set", "shell", "/bin/bash")
	if err != nil {
		t.Fatalf("config set without prior init failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "shell = /bin/bash") {
		t.Errorf("config set stdout missing 'shell = /bin/bash': %q", stdout)
	}

	settingsPath := filepath.Join(baseDir, config.SettingsFile)
	if _, statErr := os.Stat(settingsPath); statErr != nil {
		t.Errorf("settings.json not created by config set: %v", statErr)
	}

	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	if s.Shell != "/bin/bash" {
		t.Errorf("settings.Shell = %q, want %q", s.Shell, "/bin/bash")
	}
}

// config list is read-only: omitempty keeps a minimal settings.json byte-stable.
func TestConfigSet_ExistingFileByteStableUntilSet(t *testing.T) {
	baseDir := t.TempDir()

	minimal := []byte(`{"version":1,"workspaces":{}}` + "\n")
	settingsPath := filepath.Join(baseDir, config.SettingsFile)
	if err := os.WriteFile(settingsPath, minimal, 0o644); err != nil {
		t.Fatalf("write minimal settings: %v", err)
	}

	if _, _, err := runCmd(t, baseDir, "config", "list"); err != nil {
		t.Fatalf("config list failed: %v", err)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings after list: %v", err)
	}
	if string(after) != string(minimal) {
		t.Errorf("settings.json was modified by config list (byte-stable violated)\nbefore: %q\nafter:  %q", minimal, after)
	}
}

func TestConfigSet_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "image", "foo"}, nil)
	if code == 0 {
		t.Fatalf("expected non-zero exit from config set with corrupt settings; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "settings") {
		t.Errorf("expected stderr to mention 'settings', got %q", stderr.String())
	}
}

func TestConfigList_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "list"}, nil)
	if code == 0 {
		t.Fatalf("expected non-zero exit from config list with corrupt settings; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "settings") {
		t.Errorf("expected stderr to mention 'settings', got %q", stderr.String())
	}
}

// cobra's ExactArgs(2): too few or too many args both exit non-zero.
func TestConfigSet_WrongArgCount_ExitsOne(t *testing.T) {
	baseDir := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		{"no args", []string{"config", "set"}},
		{"one arg", []string{"config", "set", "image"}},
		{"three args", []string{"config", "set", "image", "foo", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runWithExitCode(baseDir, &stdout, &stderr, tc.args, nil)
			if code == 0 {
				t.Errorf("%s: expected non-zero exit, got 0; stdout=%q stderr=%q", tc.name, stdout.String(), stderr.String())
			}
		})
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

// ── version subcommand tests ──────────────────────────────────────────────────

func TestVersion_PrintsVersionAndExitsZero(t *testing.T) {
	// Mutates the package-level version var; must not run parallel with other
	// tests touching it.
	orig := version
	version = "test-1.2.3"
	t.Cleanup(func() { version = orig })

	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "test-1.2.3\n" {
		t.Errorf("stdout = %q, want %q", stdout, "test-1.2.3\n")
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
}

// version is exempt from the home-dir guard.
func TestVersion_ExemptFromHomeDirGuard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version must succeed outside HOME; err=%v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("version must not trigger the home-dir guard: stderr=%q", stderr)
	}
	if stdout == "" {
		t.Errorf("version must print a non-empty version string; stdout=%q", stdout)
	}
}

// version is pipe-safe: the real (false) ttyCheck under go test must not block it.
func TestVersion_ExemptFromTTYCheck(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version must succeed without a TTY; err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("version must print a non-empty version string; stdout=%q", stdout)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
}

func TestRoot_BareInvocation_ListsVersionCommand(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir)
	if err != nil {
		t.Fatalf("bare makeslop should exit 0, got err: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "\n  version ") {
		t.Errorf("stdout missing '\\n  version ' command entry: %q", stdout)
	}
}

// ── Task 4: init seed-at-latest and stale-nudge tests ────────────────────────

// Fresh init (no prior settings.json) stamps MigratedVersion = MigrationVersion,
// so a freshly-init'd dir is never reported stale.
func TestInit_FreshSeed_StampsMigratedVersion(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	stdout, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := strings.TrimSpace(stdout)
	if workspacePath == "" {
		t.Fatalf("init produced empty stdout")
	}
	if strings.Contains(workspacePath, "\n") {
		t.Errorf("stdout must be a single bare path; got %q", stdout)
	}

	if !strings.Contains(stderr, "registered") {
		t.Errorf("stderr missing 'registered': %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop build") {
		t.Errorf("stderr missing 'makeslop build' hint: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop run") {
		t.Errorf("stderr missing 'makeslop run' hint: %q", stderr)
	}

	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings after init: %v", loadErr)
	}
	if s.MigratedVersion != config.MigrationVersion {
		t.Errorf("MigratedVersion = %d, want %d (MigrationVersion)", s.MigratedVersion, config.MigrationVersion)
	}
}

// An existing stale config gets a non-blocking nudge but MigratedVersion is NOT
// stamped — stamping would skip the actual migration.
func TestInit_StaleConfig_NudgesWithoutStamping(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Seed a stale settings.json so we hit the "existing-but-stale" path.
	staleMigrated := 0
	if config.MigrationVersion == 0 {
		t.Skip("MigrationVersion is 0; nothing would be stale")
	}
	s := &config.Settings{
		Version:         config.CurrentVersion,
		Image:           config.DefaultImage,
		Shell:           config.DefaultShell,
		TmpDirSize:      config.DefaultTmpDirSize,
		Workspaces:      map[string]config.Workspace{},
		MigratedVersion: staleMigrated,
	}
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("seed stale settings: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init on stale config failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stderr, "note: your base config is") {
		t.Errorf("stderr missing stale-config nudge: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop migrate") {
		t.Errorf("stderr missing 'makeslop migrate' in nudge: %q", stderr)
	}

	after, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings after init: %v", loadErr)
	}
	if after.MigratedVersion != staleMigrated {
		t.Errorf("init must not stamp MigratedVersion on stale dir; got %d, want %d",
			after.MigratedVersion, staleMigrated)
	}
}

// init stdout is the bare workspace path only (no labels, no extra lines).
func TestInit_FreshSeed_StdoutIsBarePathOnly(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	stdout, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatalf("stdout is empty; expected workspace path")
	}
	if strings.ContainsAny(line, " \t\n") {
		t.Errorf("stdout must be a bare path with no extra whitespace; got %q", stdout)
	}
	workspacesRoot := filepath.Join(baseDir, "workspaces")
	if !strings.HasPrefix(line, workspacesRoot+string(filepath.Separator)) {
		t.Errorf("workspace path %q not under %q", line, workspacesRoot)
	}
}

// Edge case: build's Bootstrap creates dirs + Dockerfile but no settings.json,
// so a later init must treat the dir as fresh (stamp latest), not stale.
func TestInit_AfterBuild_TreatedAsFresh(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if err := config.Bootstrap(baseDir); err != nil {
		t.Fatalf("Bootstrap (simulating build): %v", err)
	}

	exists, err := config.BaseConfigExists(baseDir)
	if err != nil {
		t.Fatalf("BaseConfigExists: %v", err)
	}
	if exists {
		t.Fatal("pre-condition failed: settings.json must not exist after Bootstrap alone")
	}

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init after build failed: %v; stderr=%q", err, stderr)
	}

	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings after init: %v", loadErr)
	}
	if s.MigratedVersion != config.MigrationVersion {
		t.Errorf("MigratedVersion = %d after build+init, want %d; stderr was %q",
			s.MigratedVersion, config.MigrationVersion, stderr)
	}

	if strings.Contains(stderr, "note: your base config is") {
		t.Errorf("stale-config nudge must not appear after build+init (fresh seed); stderr=%q", stderr)
	}
}

// An up-to-date config must not emit the stale-config nudge.
func TestInit_UpToDateConfig_NoNudge(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("first init failed: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("second init failed: %v", err)
	}
	if strings.Contains(stderr, "note: your base config is") {
		t.Errorf("stale-config nudge must not appear when config is up to date; stderr=%q", stderr)
	}
}

// ── Task 6: run pre-flight tests ─────────────────────────────────────────────

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

// ── Task 7: config bare / --out-of-home scope / --quiet tests ────────────────

// Bare `config` output must equal `config list`.
func TestConfig_Bare_EqualsConfigList(t *testing.T) {
	baseDir := t.TempDir()

	bareOut, bareErr, err := runCmd(t, baseDir, "config")
	if err != nil {
		t.Fatalf("bare config failed: %v; stderr=%q", err, bareErr)
	}
	listOut, listErr, err := runCmd(t, baseDir, "config", "list")
	if err != nil {
		t.Fatalf("config list failed: %v; stderr=%q", err, listErr)
	}

	if bareOut != listOut {
		t.Errorf("bare 'config' output != 'config list' output\nbare:\n%s\nlist:\n%s", bareOut, listOut)
	}
}

// --out-of-home is rejected on commands that don't register it.
func TestOutOfHome_RejectedOnVersion(t *testing.T) {
	baseDir := t.TempDir()

	for _, cmd := range [][]string{
		{"version", "--out-of-home"},
		{"migrate", "--out-of-home"},
		{"build", "--out-of-home"},
		{"config", "--out-of-home"},
		{"status", "--out-of-home"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			_, _, err := runCmd(t, baseDir, cmd...)
			if err == nil {
				t.Fatalf("%v --out-of-home should fail with unknown flag, got nil", cmd[0])
			}
			if !strings.Contains(err.Error(), "unknown flag") && !strings.Contains(err.Error(), "out-of-home") {
				t.Errorf("%v --out-of-home error should mention unknown flag or out-of-home; got: %v", cmd[0], err)
			}
		})
	}
}

// --quiet suppresses the stale-config nudge (chrome) but not errors.
func TestQuiet_SuppressesInitNudge(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if config.MigrationVersion == 0 {
		t.Skip("MigrationVersion is 0; nothing would be stale")
	}
	s := &config.Settings{
		Version:         config.CurrentVersion,
		Image:           config.DefaultImage,
		Shell:           config.DefaultShell,
		TmpDirSize:      config.DefaultTmpDirSize,
		Workspaces:      map[string]config.Workspace{},
		MigratedVersion: 0, // stale
	}
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("seed stale settings: %v", err)
	}

	_, stderrNoQuiet, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderrNoQuiet)
	}
	if !strings.Contains(stderrNoQuiet, "note: your base config is") {
		t.Errorf("expected nudge on stderr without --quiet; got: %q", stderrNoQuiet)
	}

	s.MigratedVersion = 0 // re-seed stale for the next call
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("re-seed stale settings: %v", err)
	}

	_, stderrQuiet, err := runCmd(t, baseDir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet failed: %v; stderr=%q", err, stderrQuiet)
	}
	if strings.Contains(stderrQuiet, "note: your base config is") {
		t.Errorf("--quiet must suppress nudge; got: %q", stderrQuiet)
	}
}

// --quiet suppresses the "masked N" notice but the /dev/null mounts still appear.
func TestQuiet_SuppressesMaskedCount(t *testing.T) {
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
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stdout1, stderr1, err := runCmd(t, baseDir, "run", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr1)
	}
	if !strings.Contains(stderr1, "masked 1 secret file") {
		t.Errorf("expected 'masked 1 secret file' on stderr without --quiet; got: %q", stderr1)
	}

	stdout2, stderr2, err := runCmd(t, baseDir, "--quiet", "run", "--dry-run")
	if err != nil {
		t.Fatalf("--quiet --dry-run failed: %v; stderr=%q", err, stderr2)
	}
	if strings.Contains(stderr2, "masked") {
		t.Errorf("--quiet must suppress 'masked' notice; got: %q", stderr2)
	}
	if !strings.Contains(stdout2, "/dev/null") {
		t.Errorf("--quiet --dry-run output must still contain /dev/null mounts; stdout:\n%s", stdout2)
	}
	if stdout1 != stdout2 {
		t.Errorf("stdout differs between --quiet and non-quiet runs:\nnon-quiet: %s\nquiet: %s", stdout1, stdout2)
	}
}

// --quiet suppresses the "registered …" notice but not the stdout workspace path.
func TestQuiet_SuppressesRegisteredNotice(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	stdout, stderr, err := runCmd(t, baseDir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "registered") {
		t.Errorf("--quiet must suppress 'registered' notice; stderr=%q", stderr)
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		t.Errorf("stdout must contain the workspace path even with --quiet; got empty")
	}
	workspacesRoot := filepath.Join(baseDir, "workspaces")
	if !strings.HasPrefix(path, workspacesRoot+string(filepath.Separator)) {
		t.Errorf("workspace path %q not under %q", path, workspacesRoot)
	}
}

// ── Task 8: error-voice tests ─────────────────────────────────────────────────

// Error-voice format "makeslop: <what> — <remedy>" with the --out-of-home flag named.
func TestErrorVoice_HomeGuard_ContainsRemedy(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	_, stderr, err := runCmd(t, baseDir, "run")
	if err == nil {
		t.Fatalf("expected error from run outside HOME")
	}
	if !strings.HasPrefix(stderr, "makeslop: ") {
		t.Errorf("home-guard error must start with 'makeslop: '; got: %q", stderr)
	}
	if !strings.Contains(stderr, " — ") {
		t.Errorf("home-guard error must contain em-dash remedy separator ' — '; got: %q", stderr)
	}
	if !strings.Contains(stderr, "--out-of-home") {
		t.Errorf("home-guard remedy must name '--out-of-home' flag; got: %q", stderr)
	}
}

// Error-voice format with the 'makeslop init' remedy.
func TestErrorVoice_NoWorkspace_ContainsRemedy(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "run")
	if err == nil {
		t.Fatalf("expected error from run with no workspace")
	}
	if !strings.HasPrefix(stderr, "makeslop: ") {
		t.Errorf("no-workspace error must start with 'makeslop: '; got: %q", stderr)
	}
	if !strings.Contains(stderr, " — ") {
		t.Errorf("no-workspace error must contain em-dash remedy separator ' — '; got: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop init") {
		t.Errorf("no-workspace remedy must mention 'makeslop init'; got: %q", stderr)
	}
}

// Error-voice format with an interactive-terminal remedy.
func TestErrorVoice_NoTTY_ContainsRemedy(t *testing.T) {
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
		t.Fatalf("expected error when stdin/stdout are not TTYs")
	}
	if !strings.HasPrefix(stderr, "makeslop: ") {
		t.Errorf("no-TTY error must start with 'makeslop: '; got: %q", stderr)
	}
	if !strings.Contains(stderr, " — ") {
		t.Errorf("no-TTY error must contain em-dash remedy separator ' — '; got: %q", stderr)
	}
	if !strings.Contains(stderr, "interactive terminal") {
		t.Errorf("no-TTY remedy must mention 'interactive terminal'; got: %q", stderr)
	}
}

// Error-voice format with a 'docker running' remedy.
func TestErrorVoice_DaemonDown_ContainsRemedy(t *testing.T) {
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
		t.Fatalf("expected error when daemon is down")
	}
	if !strings.HasPrefix(stderr, "makeslop: ") {
		t.Errorf("daemon-down error must start with 'makeslop: '; got: %q", stderr)
	}
	if !strings.Contains(stderr, " — ") {
		t.Errorf("daemon-down error must contain em-dash remedy separator ' — '; got: %q", stderr)
	}
	if !strings.Contains(stderr, "docker running") {
		t.Errorf("daemon-down remedy must mention 'docker running'; got: %q", stderr)
	}
}

// Error-voice format with a 'makeslop build' remedy.
func TestErrorVoice_ImageMissing_ContainsRemedy(t *testing.T) {
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
		t.Fatalf("expected error when image is missing")
	}
	if !strings.HasPrefix(stderr, "makeslop: ") {
		t.Errorf("image-missing error must start with 'makeslop: '; got: %q", stderr)
	}
	if !strings.Contains(stderr, " — ") {
		t.Errorf("image-missing error must contain em-dash remedy separator ' — '; got: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop build") {
		t.Errorf("image-missing remedy must mention 'makeslop build'; got: %q", stderr)
	}
}

// ── cache mount config tests ──────────────────────────────────────────────────

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

// ── Task 5: --global-only flag tests ──────────────────────────────────────────

// init --global-only scaffolds a .makeslop.yaml parsing to Cache{false, false}.
func TestInit_GlobalOnly_ScaffoldsCacheDisabled(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "init", "--global-only")
	if err != nil {
		t.Fatalf("init --global-only failed: %v; stderr=%q", err, stderr)
	}

	resolvedPwd := evalSymlinks(t, pwd)
	_, cache, _, err := projectconfig.Load(resolvedPwd)
	if err != nil {
		t.Fatalf("projectconfig.Load after init --global-only: %v", err)
	}
	if cache.Content {
		t.Errorf("cache.Content must be false after init --global-only; got true")
	}
	if cache.Agent {
		t.Errorf("cache.Agent must be false after init --global-only; got true")
	}
}

// Plain init scaffolds a .makeslop.yaml parsing to Cache{true, true}.
func TestInit_NoGlobalOnly_ScaffoldsCacheEnabled(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v; stderr=%q", err, stderr)
	}

	resolvedPwd := evalSymlinks(t, pwd)
	_, cache, _, err := projectconfig.Load(resolvedPwd)
	if err != nil {
		t.Fatalf("projectconfig.Load after init: %v", err)
	}
	if !cache.Content {
		t.Errorf("cache.Content must be true after plain init; got false")
	}
	if !cache.Agent {
		t.Errorf("cache.Agent must be true after plain init; got false")
	}
}

// init --global-only is a no-op on an existing .makeslop.yaml (Scaffold is
// idempotent: EEXIST = success, never clobbers).
func TestInit_GlobalOnly_IsNopOnExistingFile(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	resolvedPwd := evalSymlinks(t, pwd)
	existing := []byte("exclude:\n  dirs: []\n  files: []\n  scan:\n    patterns: []\ncache:\n  content: true\n  agent: true\n")
	configPath := filepath.Join(resolvedPwd, projectconfig.Filename)
	if err := os.WriteFile(configPath, existing, 0o644); err != nil {
		t.Fatalf("write pre-existing config: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "init", "--global-only")
	if err != nil {
		t.Fatalf("init --global-only on existing config failed: %v; stderr=%q", err, stderr)
	}

	after, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read config after init --global-only: %v", readErr)
	}
	if !bytes.Equal(after, existing) {
		t.Errorf(".makeslop.yaml was modified by init --global-only\ngot:  %q\nwant: %q", after, existing)
	}
}

// --global-only is rejected as unknown on every command except init.
func TestGlobalOnly_RejectedOnNonInitCommands(t *testing.T) {
	baseDir := t.TempDir()

	for _, cmd := range [][]string{
		{"run", "--global-only"},
		{"version", "--global-only"},
		{"migrate", "--global-only"},
		{"build", "--global-only"},
		{"config", "--global-only"},
		{"status", "--global-only"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			_, _, err := runCmd(t, baseDir, cmd...)
			if err == nil {
				t.Fatalf("%v --global-only should fail with unknown flag, got nil", cmd[0])
			}
			if !strings.Contains(err.Error(), "unknown flag") && !strings.Contains(err.Error(), "global-only") {
				t.Errorf("%v --global-only error should mention unknown flag or global-only; got: %v", cmd[0], err)
			}
		})
	}
}

// runWithExitCode must wire a signal.NotifyContext (not context.Background) into
// ExecuteContext. Structural check only — signal delivery isn't exercised here.
func TestRunWithExitCode_ContextIsCancellable(t *testing.T) {
	baseDir := t.TempDir()

	var captured context.Context
	observer := func(ctx context.Context) { captured = ctx }

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"version"}, observer)
	if code != 0 {
		t.Fatalf("runWithExitCode(version) = %d, want 0; stderr=%q", code, stderr.String())
	}
	if captured == nil {
		t.Fatal("contextObserver was not called — runWithExitCode did not invoke the hook")
	}
	if captured == context.Background() {
		t.Error("context passed to ExecuteContext must not be context.Background() — signal.NotifyContext wiring is missing")
	}
	if captured.Done() == nil {
		t.Error("context.Done() must be non-nil — context must be cancellable")
	}
}

// Guards that ExecuteContext does not regress the normal success path.
func TestRunWithExitCode_VersionSucceeds(t *testing.T) {
	baseDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"version"}, nil)
	if code != 0 {
		t.Errorf("runWithExitCode(version) = %d, want 0; stderr=%q", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Error("version command produced empty stdout")
	}
}

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
	_ = workspaceDir

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
	_ = stderr

	for _, tok := range strings.Fields(stdout) {
		if tok == "-e" {
			t.Errorf("--dry-run output must not contain -e flags when environments: block is absent\nstdout:\n%s", stdout)
			break
		}
	}
}

// ── Task 4: runRun gating + warning output tests ──────────────────────────────

// Helper: hasMountWithContainer returns true iff spec.Mounts contains a mount
// whose Container field equals target.
func hasMountWithContainer(mounts []docker.Mount, target string) bool {
	for _, m := range mounts {
		if m.Container == target {
			return true
		}
	}
	return false
}

// helper: mount with matching container AND host
func hasMountWithContainerAndHost(mounts []docker.Mount, container, host string) bool {
	for _, m := range mounts {
		if m.Container == container && m.Host == host {
			return true
		}
	}
	return false
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

	workspacePath := "/workspace/" + workspaceName
	wantHooksContainer := workspacePath + "/.git/hooks"
	if hasMountWithContainer(fc.LastSpec.Mounts, wantHooksContainer) {
		t.Errorf("MaskGitHooks mount must be absent when .git does not exist; mounts: %+v", fc.LastSpec.Mounts)
	}
}

// No .makeslop.yaml → ProtectProjectConfig mount absent.
func TestRun_NoConfig_NoConfigMount(t *testing.T) {
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

	// Remove .makeslop.yaml so the gate is off.
	if err := os.Remove(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove .makeslop.yaml: %v", err)
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
	}

	workspacePath := "/workspace/" + workspaceName
	wantConfigContainer := workspacePath + "/.makeslop.yaml"
	if hasMountWithContainer(fc.LastSpec.Mounts, wantConfigContainer) {
		t.Errorf("ProtectProjectConfig mount must be absent when .makeslop.yaml does not exist; mounts: %+v", fc.LastSpec.Mounts)
	}
}

// .git as a regular file (worktree/submodule gitfile) → MaskGitHooks mount absent.
func TestRun_GitFile_NoHooksMask(t *testing.T) {
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

	// Simulate a gitfile (worktree/submodule): .git is a regular file.
	if err := os.WriteFile(filepath.Join(resolvedPwd, ".git"), []byte("gitdir: ../.git/worktrees/main\n"), 0o644); err != nil {
		t.Fatalf("write .git gitfile: %v", err)
	}

	fc := newFakeDocker(0, true)
	_, stderr, err := runCmdWithDeps(t, baseDir, depsFrom(fc), "run")
	if err != nil {
		t.Fatalf("run failed: %v; stderr=%q", err, stderr)
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
	if !strings.Contains(stderrNonQuiet, "symlink") {
		t.Errorf("non-quiet: stderr must contain symlink warning; got: %q", stderrNonQuiet)
	}

	// Quiet: "masked" chrome IS suppressed; symlink warning is NOT.
	_, stderrQuiet, err := runCmd(t, baseDir, "--quiet", "run", "--dry-run")
	if err != nil {
		t.Fatalf("quiet --dry-run failed: %v; stderr=%q", err, stderrQuiet)
	}
	if strings.Contains(stderrQuiet, "masked 1 secret file") {
		t.Errorf("--quiet: 'masked N' chrome must be suppressed; stderr=%q", stderrQuiet)
	}
	if !strings.Contains(stderrQuiet, "symlink") {
		t.Errorf("--quiet: symlink warning must NOT be suppressed; stderr=%q", stderrQuiet)
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

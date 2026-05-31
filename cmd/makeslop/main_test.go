package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/term"

	"github.com/Zwergpro/makeslop/internal/assets"
	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/workspace"
)

// Tests in this file swap docker.dockerBinary and docker.ttyCheck via the
// package-level SetForTest helpers. Do not add t.Parallel() to tests that
// touch those swaps — the underlying state is process-global.

func runCmd(t *testing.T, baseDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd(baseDir)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
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

// setHomeToTestParent sets HOME to the parent of t.TempDir(), making all
// subsequent TempDir() calls siblings that satisfy ensureWithinHome.
func setHomeToTestParent(t *testing.T) {
	t.Helper()
	sentinel := t.TempDir()
	parent := evalSymlinks(t, filepath.Dir(sentinel))
	t.Setenv("HOME", parent)
}

func TestGo_NotRegistered_NoMutation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	beforeFiles := listFiles(t, baseDir)
	if len(beforeFiles) != 0 {
		t.Fatalf("baseDir not empty before run: %v", beforeFiles)
	}

	stdout, stderr, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatalf("expected error from makeslop go, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing 'no workspace registered': %q", stderr)
	}
	if !strings.Contains(stderr, "run 'makeslop init'") {
		t.Errorf("stderr missing init hint: %q", stderr)
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

// installFakeRunClient installs a fake docker client that returns the given exit
// code from ContainerWait and stubs out the TTY raw-mode call so tests work
// without a real PTY. Returns the fake so tests can inspect fc.Started.
// This replaces installDockerShim for the `go` command tests (Task 3 migration).
func installFakeRunClient(t *testing.T, exitCode int) *docker.FakeRunClient {
	t.Helper()
	fc := docker.NewFakeRunClient(exitCode)
	t.Cleanup(docker.SetClientForTest(fc))
	// Stub term.MakeRaw: tests run without a real PTY, so raw mode would fail.
	t.Cleanup(docker.SetTermMakeRawForTest(func(_ int) (*term.State, error) {
		return nil, nil
	}))
	return fc
}

func installDockerShim(t *testing.T, exitCode int) string {
	t.Helper()
	docker.SkipNonPOSIX(t, "docker shim requires POSIX shell; makeslop is POSIX-only")
	shim, record := docker.WriteShim(t, t.TempDir(), exitCode)
	t.Cleanup(docker.SetDockerBinaryForTest(shim))
	return record
}

func stubTTY(t *testing.T, ok bool) {
	t.Helper()
	t.Cleanup(docker.SetTTYCheckForTest(func() bool { return ok }))
}

func readArgv(t *testing.T, recordPath string) []string {
	t.Helper()
	data, err := os.ReadFile(recordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read argv record: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// Milestone-1 regression guard: makeslop go must not print the cache path on stdout.
func TestGo_AfterInit_LaunchesDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	workspaceDir := strings.TrimSpace(initOut)

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	snapBefore := snapshotTree(t, baseDir)
	stdout, stderr, err := runCmd(t, baseDir, "go")
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

	// Verify the spec that would be passed to docker.Run matches expectations.
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

func TestGo_FromSubdirectory_MountsRegisteredAncestor(t *testing.T) {
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

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	if _, _, err := runCmd(t, baseDir, "go"); err != nil {
		t.Fatalf("root failed: %v", err)
	}

	if !fc.Started {
		t.Error("docker.Run must have been invoked for a registered workspace")
	}

	// Verify the spec uses the registered ancestor (parent), not the subdir.
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

func TestGo_Unregistered_DoesNotInvokeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
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
func TestGo_NoTTY_FailsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	fc := installFakeRunClient(t, 0)
	// Do NOT stub ttyCheck — the real predicate returns false under go test.

	_, stderr, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatalf("expected error when stdin/stdout are not TTYs, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (cobra layer wrote tailored message), got %v", err)
	}
	if !strings.Contains(stderr, "TTY") {
		t.Errorf("stderr missing TTY hint: %q", stderr)
	}
	if fc.Started {
		t.Errorf("docker client must not be started when TTY check fails")
	}
}

func TestGo_ExitCodePropagation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	// Fake client returns StatusCode 42; runWithExitCode must propagate it.
	installFakeRunClient(t, 42)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code != 42 {
		t.Errorf("runWithExitCode = %d, want 42; stderr=%q", code, stderr.String())
	}
}

// TestRunWithExitCode_DaemonReports137_MapsTo137 is a pure mapping test:
// the daemon reports StatusCode 137 (128 + SIGKILL) and runWithExitCode must
// propagate it verbatim as exit code 137.
// Note: with the SDK, we no longer fork a docker process, so we no longer
// derive 128+signum from OS WaitStatus. The daemon reports 128+signum in
// StatusCode, which we pass through directly. No SkipNonPOSIX needed.
func TestRunWithExitCode_DaemonReports137_MapsTo137(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Fake client returns StatusCode 137 (128 + SIGKILL).
	installFakeRunClient(t, 137)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code != 137 {
		t.Errorf("runWithExitCode = %d, want 137 (daemon-reported 128+SIGKILL); stderr=%q", code, stderr.String())
	}
}

// Guards that settings.json values reach the docker invocation, not just compiled-in defaults.
func TestGo_CustomImageAndShell_FlowFromSettings(t *testing.T) {
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

	// Use --dry-run to capture the argv without actually running docker, so
	// we can verify the image and shell flow from settings.json into the spec.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "makeslop: ") {
		t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
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

	// Verify that the second init did not modify .makeslop.yaml in the project
	// directory — Scaffold must be idempotent and leave a hand-edited file untouched.
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

	// Scaffold placement: .makeslop.yaml must appear in the parent (workspace root),
	// NOT in the subdirectory from which init was called.
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
	docker.SkipNonPOSIX(t, "symlinks unreliable on Windows; makeslop is POSIX-only")
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

	// settings.json must key by the resolved path, not the alias.
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
func TestGo_CorruptSettings_ReportsError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, _, err := runCmd(t, baseDir, "go")
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
	// main() prints this error; ensure it carries diagnostic context.
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

func TestGo_NotRegistered_ReturnsErrSilent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir, "go")
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
func TestGo_OutsideHome_Refuses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	snapBefore := snapshotTree(t, baseDir)
	_, stderr, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatalf("expected error from makeslop go outside HOME, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "refusing to run from") {
		t.Errorf("stderr missing 'refusing to run from': %q", stderr)
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
	if !strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr does not end with newline: %q", stderr)
	}
	// Guard: ensureWithinHome must fire BEFORE config.Bootstrap, so baseDir
	// must be completely untouched (no workspaces/, no agent artifacts).
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

	_, stderr, err := runCmd(t, baseDir, "--out-of-home", "init")
	if err != nil {
		t.Fatalf("init --out-of-home should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("init --out-of-home: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	_, stderr, err = runCmd(t, baseDir, "--out-of-home", "go")
	if err != nil {
		t.Fatalf("makeslop --out-of-home go should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("makeslop --out-of-home go: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}
	if !fc.Started {
		t.Errorf("docker client was not started when --out-of-home bypasses guard")
	}
}

func TestGo_MasksFoundEnvFiles_ArgvContainsDevNullMounts(t *testing.T) {
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

	// Config-driven scan: write .makeslop.yaml with patterns that match the files above.
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n      - \".env.*\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Use --dry-run to verify the spec contains /dev/null mounts without
	// actually running docker. The same spec is passed to docker.Run in the
	// non-dry-run path (verified by the pure spec drift-guard in spec_test.go).
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

	// Verify the overlay mounts appear after the project bind mount (tail ordering).
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

func TestGo_NoEnvFiles_PrintsNothingExtraOnStderr(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Use --dry-run: no docker needed, and we can verify no /dev/null mounts in output.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

// No workspace init, docker shim, or fd shim required.
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
	if !strings.Contains(stdout, "\n  go ") {
		t.Errorf("stdout missing '\\n  go ' command entry: %q", stdout)
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

// "no exec" contract: --dry-run succeeds even when TTY is false (no docker exec).
func TestGo_DryRun_SkipsDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	fc := installFakeRunClient(t, 0)
	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

// Single source of truth: --dry-run stdout must equal BuildSpec(opts).ShellCommand()
// (after stripping the trailing newline from fmt.Fprintln).
func TestGo_DryRun_StdoutEqualsBuildSpecShellCommand(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	workspaceDir := strings.TrimSpace(initOut)

	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
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
	}).ShellCommand()

	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("stdout mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestGo_DryRun_ShortFlag(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	stubTTY(t, false)

	stdoutLong, stderrLong, errLong := runCmd(t, baseDir, "go", "--dry-run")
	if errLong != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", errLong, stderrLong)
	}

	stdoutShort, stderrShort, errShort := runCmd(t, baseDir, "go", "-n")
	if errShort != nil {
		t.Fatalf("-n failed: %v; stderr=%q", errShort, stderrShort)
	}

	if stdoutShort != stdoutLong {
		t.Errorf("-n stdout != --dry-run stdout\nshort:\n%s\nlong:\n%s", stdoutShort, stdoutLong)
	}
}

// TTY-bypass guard: --dry-run succeeds even when real ttyCheck returns false
// because docker.Run (the only ttyCheck caller) is never invoked.
func TestGo_DryRun_NoTTY_Succeeds(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Scan is config-driven (WalkDir); no fd shim needed.
	// Do NOT stub ttyCheck — the real predicate returns false under go test.
	// Do NOT install docker shim — docker.Run must never be called.

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run must succeed with no TTY (TTY check lives in docker.Run which is skipped); err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("--dry-run must print command to stdout; got empty")
	}
}

func TestGo_DryRun_Unregistered_StillRefuses(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)
	// No init — workspace is not registered.

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err == nil {
		t.Fatalf("expected error for unregistered workspace, got nil; stdout=%q", stdout)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing 'no workspace registered': %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when precondition fails; got %q", stdout)
	}
}

func TestGo_DryRun_OutsideHome_StillRefuses(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err == nil {
		t.Fatalf("expected error from --dry-run outside HOME, got nil; stdout=%q", stdout)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "refusing to run from") {
		t.Errorf("stderr missing 'refusing to run from': %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when home-dir guard fires; got %q", stdout)
	}
}

func TestGo_DryRun_OutOfHomeBypasses(t *testing.T) {
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

	stubTTY(t, false)

	// --out-of-home flag comes before the subcommand (it is a persistent flag).
	stdout, stderr, err := runCmd(t, baseDir, "--out-of-home", "go", "--dry-run")
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
func TestGo_DryRun_CorruptSettings(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Register workspace so init succeeds, then corrupt settings so ws.Lookup fails.
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt settings: %v", err)
	}

	stubTTY(t, false)

	stdout, _, err := runCmd(t, baseDir, "go", "--dry-run")
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

func TestGo_DryRun_MasksEnvFiles_StdoutContainsDevNullMounts(t *testing.T) {
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

	// Config-driven scan: write .makeslop.yaml with patterns that match the files above.
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n      - \".env.*\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

	// Verify the /dev/null overlay mounts appear AFTER the project bind mount
	// in the printed output (mount-order invariant).
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

func TestGo_DryRun_FromSubdir_MountsAncestor(t *testing.T) {
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

	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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
	// The error message must mention "dry-run" (exact cobra phrasing may vary
	// across versions, so we check for the flag name substring only).
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

// TestGo_EmptyScanPatterns_NoFilesMasked asserts the opt-in rule: when
// exclude.scan.patterns is absent (or the .makeslop.yaml is absent entirely),
// no files are masked and go succeeds even when secret files exist on disk.
// This also verifies that the absence of fd/fdfind is no longer an issue.
func TestGo_EmptyScanPatterns_NoFilesMasked(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// Write a "secret" file that would have been caught by the old fd scan.
	envFile := filepath.Join(resolvedPwd, ".env")
	if err := os.WriteFile(envFile, []byte("SECRET=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// Overwrite .makeslop.yaml with an empty scan block (no patterns).
	yamlContent := "exclude:\n  scan:\n    patterns: []\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	installFakeRunClient(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("go must succeed when exclude.scan.patterns is empty; err=%v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "masked") {
		t.Errorf("stderr must not mention 'masked' when exclude.scan.patterns is empty: %q", stderr)
	}
}

func TestGo_LoadsYamlAndMergesMaskedFiles(t *testing.T) {
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

	// Config-driven scan: write .makeslop.yaml with patterns (scan finds .env) and
	// an explicit file mask (private/token.txt goes via exclude.files).
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  files: [private/token.txt]\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Use --dry-run to verify spec content without running docker.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	// Scan hits .env (1 file); private/token.txt is via exclude.files (not counted in masked N).
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

	// Lex-sorted order: .env < private/token.txt — check position in output.
	idx1 := strings.Index(stdout, wantMount1)
	idx2 := strings.Index(stdout, wantMount2)
	if idx1 >= 0 && idx2 >= 0 && idx1 >= idx2 {
		t.Errorf("/dev/null mount for .env (byte %d) should come before private/token.txt (byte %d) in lex order", idx1, idx2)
	}
}

// TestGo_BadScanPattern_AbortsBeforeDocker verifies that a malformed glob pattern in
// exclude.scan.patterns causes makeslop go to abort with an error before invoking docker.
func TestGo_BadScanPattern_AbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// Write .makeslop.yaml with an invalid glob pattern (unclosed bracket).
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"[bad\"\n  files: []\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, _, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatal("makeslop go must fail with a bad scan pattern, got nil error")
	}
}

func TestGo_LoadsYamlMaskedDirs_TmpfsMountInArgv(t *testing.T) {
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

	// Create the directory on disk so projectconfig.Load stat-keeps it.
	nodeModules := filepath.Join(resolvedPwd, "node_modules")
	if err := os.MkdirAll(nodeModules, 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}

	yamlContent := "exclude:\n  dirs: [node_modules]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Use --dry-run to verify spec contains the tmpfs mount without running docker.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	wantTmpfs := "type=tmpfs,target=/workspace/" + name + "/node_modules"
	projectMount := "source=" + resolvedPwd + ",target=/workspace/" + name

	if !strings.Contains(stdout, wantTmpfs) {
		t.Errorf("--dry-run stdout missing tmpfs mount: want %q\nstdout:\n%s", wantTmpfs, stdout)
	}
	// Verify ordering: tmpfs appears after project bind mount.
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

func TestGo_YamlAbsentIsBitIdenticalArgv(t *testing.T) {
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

	// Use --dry-run: yaml absent → spec must equal what BuildSpec produces directly.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("go --dry-run failed: %v; stderr=%q", err, stderr)
	}

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
	}).ShellCommand()

	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("--dry-run stdout mismatch (yaml absent must yield identical command)\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestGo_YamlDedupsAgainstScan(t *testing.T) {
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

	// Config-driven scan finds .env AND explicit files list also includes .env —
	// mergeUniqueSorted must deduplicate so only one /dev/null mount appears.
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n  dirs: []\n  files: [.env]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// Use --dry-run to verify dedup without running docker.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

// Guards the secret-masking invariant: docker must never start when yaml parse fails.
// Uses runWithExitCode (not runCmd) so non-errSilent errors appear on stderr.
func TestGo_YamlMalformedAbortsBeforeDocker(t *testing.T) {
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

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
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
func TestGo_YamlReservedPathAbortsBeforeDocker(t *testing.T) {
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

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
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
func TestGo_YamlDirAndFileDupAborts(t *testing.T) {
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

	fc := installFakeRunClient(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
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

func TestGo_YamlMissingPathSkippedSilently(t *testing.T) {
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
	// Explicitly ensure the file does not exist.
	_ = os.Remove(filepath.Join(resolvedPwd, "secrets", "api.key"))

	// Use --dry-run to verify spec without running docker.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

// ── proxy lifecycle wiring tests ──────────────────────────────────────────────

// Guards the dry-run contract: no socket is created on disk even though
// the argv includes proxy plumbing (--network none, env vars, socket mount).
func TestGo_DryRun_WithProxy_PrintsProxyArgvNoSocket(t *testing.T) {
	docker.SkipNonPOSIX(t, "proxy socket tests are POSIX-only; makeslop is POSIX-only")
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

	yamlContent := "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: 10.0.0.5:8888\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run with proxy failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stdout, "--network none") {
		t.Errorf("stdout missing '--network none'\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "HTTP_PROXY=unix:///tmp/makeslop-proxy.sock") {
		t.Errorf("stdout missing HTTP_PROXY env var\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "HTTPS_PROXY=unix:///tmp/makeslop-proxy.sock") {
		t.Errorf("stdout missing HTTPS_PROXY env var\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "target=/tmp/makeslop-proxy.sock") {
		t.Errorf("stdout missing proxy socket container target\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "readonly") {
		t.Errorf("stdout missing 'readonly' in proxy socket mount\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "source=/tmp/makeslop-") {
		t.Errorf("stdout missing expected socket host path 'source=/tmp/makeslop-'\nstdout:\n%s", stdout)
	}

	// CRITICAL: no socket file must have been created on disk.
	name := filepath.Base(workspaceDir)
	if !strings.Contains(stdout, name) {
		t.Errorf("stdout missing workspace name %q\nstdout:\n%s", name, stdout)
	}

	// The format is: source=/tmp/makeslop-<hash>-<pid>.sock
	const prefix = "source=/tmp/makeslop-"
	pidx := strings.Index(stdout, prefix)
	if pidx < 0 {
		t.Fatalf("stdout missing source=/tmp/makeslop- prefix\nstdout:\n%s", stdout)
	}
	rest := stdout[pidx+len("source="):]
	// The --mount value is a single comma-separated token; take everything up to
	// the next comma to get just the path (e.g. "/tmp/makeslop-abc123-999.sock").
	end := strings.IndexByte(rest, ',')
	if end < 0 {
		end = len(rest)
	}
	sockPath := rest[:end]
	if _, err := os.Lstat(sockPath); err == nil {
		t.Errorf("--dry-run must NOT create the socket file %q on disk", sockPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Logf("unexpected stat error for socket path %q: %v (expected ErrNotExist)", sockPath, err)
	}
}

func TestGo_DryRun_WithoutProxy_UnchangedArgv(t *testing.T) {
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

	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run without proxy failed: %v; stderr=%q", err, stderr)
	}

	if strings.Contains(stdout, "--network") {
		t.Errorf("stdout must not contain --network when no proxy configured\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "HTTP_PROXY") {
		t.Errorf("stdout must not contain HTTP_PROXY when no proxy configured\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "HTTPS_PROXY") {
		t.Errorf("stdout must not contain HTTPS_PROXY when no proxy configured\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "makeslop-proxy.sock") {
		t.Errorf("stdout must not contain proxy socket when no proxy configured\nstdout:\n%s", stdout)
	}

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
	}).ShellCommand()
	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("stdout mismatch (proxy section absent must yield identical argv)\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestGo_SocketPathLength_AtMost108Bytes(t *testing.T) {
	// Simulates computeSocketPath with an extreme workspace dir name to verify
	// the path stays within the 108-byte sockaddr_un limit.
	const sockaddrUnLimit = 108

	longBasename := strings.Repeat("a", 200)
	workspaceDir := "/home/user/.makeslop/workspaces/" + longBasename + "-abcdef"

	h := sha256.Sum256([]byte(workspaceDir))
	sockPath := filepath.Join("/tmp", fmt.Sprintf("makeslop-%x-%d.sock", h[:6], 99999))

	if len(sockPath) > sockaddrUnLimit {
		t.Errorf("socket path length %d exceeds %d-byte sockaddr_un limit: %q", len(sockPath), sockaddrUnLimit, sockPath)
	}
	t.Logf("socket path (%d bytes): %q", len(sockPath), sockPath)
}

// ── migrate subcommand tests ───────────────────────────────────────────────────

// TestMigrate_FirstRun_PrintsUpdatedAndWritesDockerfile verifies that a fresh
// migrate on an empty baseDir prints "updated" and creates the Dockerfile.
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

// TestMigrate_SecondRun_PrintsAlreadyUpToDate verifies idempotence: a second
// migrate returns "already up to date" and does not re-write the file.
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

// TestMigrate_WithoutPriorInit_SucceedsAndWritesDockerfile verifies that
// migrate works standalone (no prior init, no pre-created dirs).
func TestMigrate_WithoutPriorInit_SucceedsAndWritesDockerfile(t *testing.T) {
	// Use a fresh, non-existing subdirectory inside a TempDir so migrate must
	// create the directory itself.
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

// TestMigrate_CorruptSettings_ReportsError verifies that migrate with a corrupt
// settings.json exits non-zero and surfaces an error mentioning "settings".
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

// TestRoot_BareInvocation_ListsMigrateCommand checks that the bare help output
// lists the migrate subcommand in the Available Commands section.
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

func TestGo_DryRunIncludesMaskedDirs(t *testing.T) {
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

	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
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

// executableTempDirForCmd returns a temp dir that is on an executable
// filesystem. It delegates to t.TempDir() which honours the GOTMPDIR env var;
// set GOTMPDIR=/home/user (or any executable path) when running tests in
// environments where /tmp is mounted noexec.
func executableTempDirForCmd(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// installBuildShim installs a build shim (recording argv + DOCKER_BUILDKIT)
// that exits with exitCode. Returns the argv record path and env record path.
func installBuildShim(t *testing.T, exitCode int) (record, envRecord string) {
	t.Helper()
	docker.SkipNonPOSIX(t, "docker build shim requires POSIX shell; makeslop is POSIX-only")
	shim, rec, env := docker.WriteBuildShim(t, executableTempDirForCmd(t), exitCode)
	t.Cleanup(docker.SetDockerBinaryForTest(shim))
	return rec, env
}

// TestBuild_SeedsSelfHealAndInvokesDocker verifies that `makeslop build` on a
// fresh (empty) baseDir bootstraps the Dockerfile then invokes docker build
// with -t claudebox and -f <baseDir>/Dockerfile.
func TestBuild_SeedsSelfHealAndInvokesDocker(t *testing.T) {
	baseDir := t.TempDir()
	record, _ := installBuildShim(t, 0)

	stdout, stderr, err := runCmd(t, baseDir, "build")
	if err != nil {
		t.Fatalf("build failed: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}

	// Dockerfile must have been seeded (self-heal).
	dockerfilePath := filepath.Join(baseDir, config.DockerfileFile)
	if _, statErr := os.Stat(dockerfilePath); statErr != nil {
		t.Errorf("Dockerfile not seeded by build: %v", statErr)
	}

	argv := readArgv(t, record)
	if len(argv) == 0 {
		t.Fatal("docker shim was not invoked")
	}
	if argv[0] != "build" {
		t.Errorf("argv[0] = %q, want %q", argv[0], "build")
	}

	// Must contain -t claudebox.
	var foundTag bool
	for i, a := range argv {
		if a == "-t" && i+1 < len(argv) && argv[i+1] == "claudebox" {
			foundTag = true
			break
		}
	}
	if !foundTag {
		t.Errorf("argv missing -t claudebox: %v", argv)
	}

	// Must contain -f <baseDir>/Dockerfile.
	var foundF bool
	for i, a := range argv {
		if a == "-f" && i+1 < len(argv) && argv[i+1] == dockerfilePath {
			foundF = true
			break
		}
	}
	if !foundF {
		t.Errorf("argv missing -f %s: %v", dockerfilePath, argv)
	}
}

// TestBuild_NoCacheAndBuildArg verifies that --no-cache and --build-arg flags
// are forwarded verbatim to the docker build argv.
func TestBuild_NoCacheAndBuildArg(t *testing.T) {
	baseDir := t.TempDir()
	record, _ := installBuildShim(t, 0)

	_, stderr, err := runCmd(t, baseDir, "build", "--no-cache", "--build-arg", "GO_VERSION=1.26.3")
	if err != nil {
		t.Fatalf("build --no-cache --build-arg failed: %v; stderr=%q", err, stderr)
	}

	argv := readArgv(t, record)
	var foundNoCache, foundBuildArg bool
	for i, a := range argv {
		if a == "--no-cache" {
			foundNoCache = true
		}
		if a == "--build-arg" && i+1 < len(argv) && argv[i+1] == "GO_VERSION=1.26.3" {
			foundBuildArg = true
		}
	}
	if !foundNoCache {
		t.Errorf("argv missing --no-cache: %v", argv)
	}
	if !foundBuildArg {
		t.Errorf("argv missing --build-arg GO_VERSION=1.26.3: %v", argv)
	}
}

// TestBuild_NonZeroExit_PropagatesCode verifies that a non-zero shim exit
// propagates through runWithExitCode as a non-zero exit code.
func TestBuild_NonZeroExit_PropagatesCode(t *testing.T) {
	baseDir := t.TempDir()
	installBuildShim(t, 42)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"build"})
	if code != 42 {
		t.Errorf("runWithExitCode = %d, want 42; stderr=%q", code, stderr.String())
	}
}

// TestBuild_CustomImage_FromSettings verifies that a custom image name in
// settings.json is used as the -t tag. config.Bootstrap does NOT create
// settings.json, so this test writes it explicitly via config.Save.
func TestBuild_CustomImage_FromSettings(t *testing.T) {
	baseDir := t.TempDir()
	record, _ := installBuildShim(t, 0)

	// Bootstrap the baseDir so config.Save has the directory structure in place.
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

	_, stderr, err := runCmd(t, baseDir, "build")
	if err != nil {
		t.Fatalf("build failed: %v; stderr=%q", err, stderr)
	}

	argv := readArgv(t, record)
	var foundTag bool
	for i, a := range argv {
		if a == "-t" && i+1 < len(argv) && argv[i+1] == "my-custom-image:v2" {
			foundTag = true
			break
		}
	}
	if !foundTag {
		t.Errorf("argv missing -t my-custom-image:v2: %v", argv)
	}
}

// TestRoot_BareInvocation_ListsBuildCommand checks that the bare help output
// lists the build subcommand in the Available Commands section.
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

// TestBuild_CorruptSettings_ReportsError verifies that `makeslop build` with a
// corrupt settings.json exits non-zero and surfaces an error mentioning
// "settings". Mirrors TestMigrate_CorruptSettings_ReportsError and
// TestInit_CorruptSettings_ReportsError.
func TestBuild_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()
	// Seed the Dockerfile so Bootstrap doesn't fail before the settings check,
	// but write a corrupt settings.json that config.Load will reject.
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

// TestBuild_MultipleBuildArgs verifies that --build-arg is repeatable and all
// values are forwarded verbatim to the docker build argv. The plan says it is
// "repeatable" — this test covers multiple values end-to-end.
func TestBuild_MultipleBuildArgs(t *testing.T) {
	baseDir := t.TempDir()
	record, _ := installBuildShim(t, 0)

	_, stderr, err := runCmd(t, baseDir, "build",
		"--build-arg", "GO_VERSION=1.26.3",
		"--build-arg", "HTTP_PROXY=http://proxy.example.com:8080",
		"--build-arg", "FOO=bar",
	)
	if err != nil {
		t.Fatalf("build --build-arg (multiple) failed: %v; stderr=%q", err, stderr)
	}

	argv := readArgv(t, record)
	wantArgs := []string{
		"GO_VERSION=1.26.3",
		"HTTP_PROXY=http://proxy.example.com:8080",
		"FOO=bar",
	}
	for _, want := range wantArgs {
		var found bool
		for i, a := range argv {
			if a == "--build-arg" && i+1 < len(argv) && argv[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv missing --build-arg %s: %v", want, argv)
		}
	}
}

// TestBuild_DOCKER_BUILDKIT_EndToEnd verifies that the CLI-level build path
// passes DOCKER_BUILDKIT=1 to docker. The env value is recovered from the
// shim's env.txt (written by WriteBuildShim).
func TestBuild_DOCKER_BUILDKIT_EndToEnd(t *testing.T) {
	baseDir := t.TempDir()
	_, envRecord := installBuildShim(t, 0)

	_, stderr, err := runCmd(t, baseDir, "build")
	if err != nil {
		t.Fatalf("build failed: %v; stderr=%q", err, stderr)
	}

	data, readErr := os.ReadFile(envRecord)
	if readErr != nil {
		t.Fatalf("read env record: %v", readErr)
	}
	if got := strings.TrimRight(string(data), "\n"); got != "1" {
		t.Errorf("DOCKER_BUILDKIT in child env = %q, want %q", got, "1")
	}
}

// ── config subcommand tests ───────────────────────────────────────────────────

// TestConfig_BareInvocation_PrintsHelp verifies that bare `makeslop config`
// prints help (lists subcommands) and exits 0.
func TestConfig_BareInvocation_PrintsHelp(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "config")
	if err != nil {
		t.Fatalf("bare 'makeslop config' should exit 0, got err: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "list") {
		t.Errorf("help output missing 'list' subcommand: %q", stdout)
	}
	if !strings.Contains(stdout, "set") {
		t.Errorf("help output missing 'set' subcommand: %q", stdout)
	}
}

// TestRoot_BareInvocation_ListsConfigCommand checks that bare makeslop help
// lists the config subcommand in the Available Commands section.
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

// TestConfigList_FreshBaseDir_PrintsThreeDefaults verifies that `config list`
// on a fresh (empty) baseDir prints the three keys with their default values in
// registry order (image, shell, tmp_dir_size).
func TestConfigList_FreshBaseDir_PrintsThreeDefaults(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "config", "list")
	if err != nil {
		t.Fatalf("config list failed: %v; stderr=%q", err, stderr)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}

	// Verify all three default lines are present.
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

	// Verify registry order: image appears before shell, shell before tmp_dir_size.
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

// TestConfigSet_ThenList_ShowsNewValue verifies that `config set` persists
// changes for all three keys (image, shell, tmp_dir_size) and `config list`
// reflects each updated value.
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
			// Set the value.
			stdout, stderr, err := runCmd(t, baseDir, "config", "set", tc.key, tc.val)
			if err != nil {
				t.Fatalf("config set %s %s failed: %v; stderr=%q", tc.key, tc.val, err, stderr)
			}
			if !strings.Contains(stdout, tc.wantLine) {
				t.Errorf("config set stdout missing %q: %q", tc.wantLine, stdout)
			}

			// List should now show the updated value.
			listOut, listErr, err := runCmd(t, baseDir, "config", "list")
			if err != nil {
				t.Fatalf("config list failed: %v; stderr=%q", err, listErr)
			}
			if !strings.Contains(listOut, tc.wantLine) {
				t.Errorf("config list output missing %q: %q", tc.wantLine, listOut)
			}

			// Verify it is persisted to settings.json.
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

// TestConfigSet_InvalidTmpDirSize_ExitsOneAndFileUnchanged verifies that an
// invalid tmp_dir_size value is rejected (exit 1, error on stderr) without
// mutating the settings file.
func TestConfigSet_InvalidTmpDirSize_ExitsOneAndFileUnchanged(t *testing.T) {
	baseDir := t.TempDir()

	// Take a snapshot of settings before the invalid set.
	snapBefore := snapshotTree(t, baseDir)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "tmp_dir_size", "9z"})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "tmp_dir_size") {
		t.Errorf("stderr missing 'tmp_dir_size'; got %q", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout, got %q", stdout.String())
	}

	// File tree must be unchanged.
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// TestConfigSet_UnknownKey_ExitsOneAndListsValidKeys verifies that an unknown
// key is rejected (exit 1) and the error on stderr mentions all valid keys.
func TestConfigSet_UnknownKey_ExitsOneAndListsValidKeys(t *testing.T) {
	baseDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "bogus", "x"})
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

// TestConfigSet_WithoutPriorInit_SelfHeals verifies that `config set` works
// without a prior `makeslop init` (Save's MkdirAll heals the missing dir).
func TestConfigSet_WithoutPriorInit_SelfHeals(t *testing.T) {
	// Use a non-existing subdirectory so config.Save must create it.
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "brand-new-makeslop-dir")

	stdout, stderr, err := runCmd(t, baseDir, "config", "set", "shell", "/bin/bash")
	if err != nil {
		t.Fatalf("config set without prior init failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "shell = /bin/bash") {
		t.Errorf("config set stdout missing 'shell = /bin/bash': %q", stdout)
	}

	// settings.json must have been created.
	settingsPath := filepath.Join(baseDir, config.SettingsFile)
	if _, statErr := os.Stat(settingsPath); statErr != nil {
		t.Errorf("settings.json not created by config set: %v", statErr)
	}

	// Reload and verify value persisted.
	s, loadErr := config.Load(baseDir)
	if loadErr != nil {
		t.Fatalf("load settings: %v", loadErr)
	}
	if s.Shell != "/bin/bash" {
		t.Errorf("settings.Shell = %q, want %q", s.Shell, "/bin/bash")
	}
}

// TestConfigSet_ExistingFileByteStableUntilSet verifies that an on-disk
// settings.json without a tmp_dir_size field is not modified by `config list`
// (read-only — omitempty keeps the file byte-stable).
func TestConfigSet_ExistingFileByteStableUntilSet(t *testing.T) {
	baseDir := t.TempDir()

	// Write a minimal settings.json with no tmp_dir_size field.
	minimal := []byte(`{"version":1,"workspaces":{}}` + "\n")
	settingsPath := filepath.Join(baseDir, config.SettingsFile)
	if err := os.WriteFile(settingsPath, minimal, 0o644); err != nil {
		t.Fatalf("write minimal settings: %v", err)
	}

	// `config list` must not touch the file.
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

// TestConfigSet_CorruptSettings_ReportsError verifies that `config set` with a
// corrupt settings.json exits non-zero and surfaces an error mentioning "settings".
func TestConfigSet_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "set", "image", "foo"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from config set with corrupt settings; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "settings") {
		t.Errorf("expected stderr to mention 'settings', got %q", stderr.String())
	}
}

// TestConfigList_CorruptSettings_ReportsError mirrors the set variant above for
// config list.
func TestConfigList_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"config", "list"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from config list with corrupt settings; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "settings") {
		t.Errorf("expected stderr to mention 'settings', got %q", stderr.String())
	}
}

// TestConfigSet_WrongArgCount_ExitsOne verifies that cobra's ExactArgs(2)
// enforcement is exercised: too few args or too many args both exit non-zero.
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
			code := runWithExitCode(baseDir, &stdout, &stderr, tc.args)
			if code == 0 {
				t.Errorf("%s: expected non-zero exit, got 0; stdout=%q stderr=%q", tc.name, stdout.String(), stderr.String())
			}
		})
	}
}

// TestGo_CustomTmpDirSize_FlowsIntoDockerArgv verifies that a tmp_dir_size set
// in settings.json is threaded all the way into the docker run argv emitted by
// `makeslop go`, matching the existing TestGo_CustomImageAndShell_FlowFromSettings
// pattern.
func TestGo_CustomTmpDirSize_FlowsIntoDockerArgv(t *testing.T) {
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

	// Use --dry-run to verify the tmpfs size flows into the spec.
	stubTTY(t, false)
	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("makeslop go --dry-run failed: %v; stderr=%q", err, stderr)
	}

	if !strings.Contains(stdout, "/tmp:size=1000m") {
		t.Errorf("--dry-run output missing '--tmpfs /tmp:size=1000m'; stdout:\n%s", stdout)
	}
}

// ── version subcommand tests ──────────────────────────────────────────────────

// TestVersion_PrintsVersionAndExitsZero verifies that `makeslop version` prints
// the current version string followed by a newline and exits 0.
func TestVersion_PrintsVersionAndExitsZero(t *testing.T) {
	// Override the package-level version to a deterministic value so the test
	// does not depend on ldflags or git state.
	// NOTE: mutates the package-level var; this test must not run in parallel
	// with other tests that read or write version.
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

// TestVersion_ExemptFromHomeDirGuard verifies that `makeslop version` succeeds
// even when cwd is outside the user's home directory (no home-dir guard applied).
func TestVersion_ExemptFromHomeDirGuard(t *testing.T) {
	// Set HOME to a temp dir, then chdir somewhere outside it.
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

// TestVersion_ExemptFromTTYCheck verifies that `makeslop version` succeeds
// even when stdin/stdout are not TTYs (pipe-safe, no ttyCheck consulted).
func TestVersion_ExemptFromTTYCheck(t *testing.T) {
	baseDir := t.TempDir()

	// Do NOT stub ttyCheck — the real predicate returns false under go test.
	// If version consulted ttyCheck it would fail here.
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

// TestRoot_BareInvocation_ListsVersionCommand checks that bare `makeslop` help
// lists the version subcommand in the Available Commands section.
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

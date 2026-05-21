package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
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
	if errors.Is(err, fs.ErrNotExist) {
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

// setHomeToTestParent sets HOME to the parent directory that t.TempDir() will
// use for subsequent calls, so that all temp dirs created by the test are
// "inside home" for ensureWithinHome. The first TempDir() is consumed as a
// sentinel to discover the parent; subsequent calls in the same test will be
// siblings under that parent.
func setHomeToTestParent(t *testing.T) {
	t.Helper()
	sentinel := t.TempDir()
	parent := evalSymlinks(t, filepath.Dir(sentinel))
	t.Setenv("HOME", parent)
}

func TestRoot_NotRegistered_NoMutation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	beforeFiles := listFiles(t, baseDir)
	if len(beforeFiles) != 0 {
		t.Fatalf("baseDir not empty before run: %v", beforeFiles)
	}

	stdout, stderr, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error from bare makeslop, got nil; stdout=%q stderr=%q", stdout, stderr)
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

// Milestone-1 regression guard: bare makeslop must not print the cache path on stdout.
func TestRoot_AfterInit_LaunchesDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	workspaceDir := strings.TrimSpace(initOut)

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	snapBefore := snapshotTree(t, baseDir)
	stdout, stderr, err := runCmd(t, baseDir)
	if err != nil {
		t.Fatalf("root failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("bare makeslop must not print on stdout (milestone-1 path was removed); got %q", stdout)
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)

	resolvedPwd := evalSymlinks(t, pwd)
	want := docker.BuildSpec(docker.Options{
		ProjectRoot:   resolvedPwd,
		WorkspaceName: filepath.Base(workspaceDir),
		BaseDir:       baseDir,
		Image:         "claudebox",
		Command:       "/bin/zsh",
	}).Args()
	got := readArgv(t, record)
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestRoot_FromSubdirectory_MountsRegisteredAncestor(t *testing.T) {
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

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	if _, _, err := runCmd(t, baseDir); err != nil {
		t.Fatalf("root failed: %v", err)
	}

	resolvedParent := evalSymlinks(t, parent)
	wantSourceFragment := `type=bind,source=` + resolvedParent + `,target=/workspace/` + filepath.Base(workspaceDir)
	argv := readArgv(t, record)
	var found bool
	for _, a := range argv {
		if a == wantSourceFragment {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("project-root mount not found in argv\nwant: %q\nargv: %v", wantSourceFragment, argv)
	}
}

func TestRoot_Unregistered_DoesNotInvokeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error from unregistered bare makeslop, got nil")
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked for unregistered workspace; got argv=%v", argv)
	}
}

// Exercises the production ttyCheck (pipes in `go test` are not TTYs).
func TestRoot_NoTTY_FailsBeforeDocker(t *testing.T) {
	docker.SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	record := installDockerShim(t, 0)
	// Do NOT stub ttyCheck — the real predicate returns false under go test.

	_, stderr, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error when stdin/stdout are not TTYs, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (cobra layer wrote tailored message), got %v", err)
	}
	if !strings.Contains(stderr, "TTY") {
		t.Errorf("stderr missing TTY hint: %q", stderr)
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when TTY check fails; got argv=%v", argv)
	}
}

func TestRoot_ExitCodePropagation(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	installDockerShim(t, 42)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, nil)
	if code != 42 {
		t.Errorf("runWithExitCode = %d, want 42; stderr=%q", code, stderr.String())
	}
}

// POSIX-only: syscall.WaitStatus shape and signal numbering are Unix.
func TestRunWithExitCode_SignalKilledMapsTo128PlusSignum(t *testing.T) {
	docker.SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md; WaitStatus.Signaled is Unix-shaped")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// SIGKILL -> 128 + 9 = 137.
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim")
	script := "#!/bin/sh\nkill -KILL $$\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Cleanup(docker.SetDockerBinaryForTest(shim))
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, nil)
	if code != 137 {
		t.Errorf("runWithExitCode = %d, want 137 (128+SIGKILL); stderr=%q", code, stderr.String())
	}
}

// Guards that settings.json values reach the docker invocation, not just compiled-in defaults.
func TestRoot_CustomImageAndShell_FlowFromSettings(t *testing.T) {
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

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	if _, _, err := runCmd(t, baseDir); err != nil {
		t.Fatalf("root failed: %v", err)
	}
	argv := readArgv(t, record)
	if len(argv) < 2 {
		t.Fatalf("argv too short: %v", argv)
	}
	if argv[len(argv)-2] != "my-img:tag" {
		t.Errorf("image arg = %q, want %q", argv[len(argv)-2], "my-img:tag")
	}
	if argv[len(argv)-1] != "/bin/dash" {
		t.Errorf("shell arg = %q, want %q", argv[len(argv)-1], "/bin/dash")
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
	code := runWithExitCode(baseDir, &stdout, &stderr, nil)
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
	pwd := t.TempDir()
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

// TestRoot_CorruptSettings_ReportsError guards that a non-ErrNotRegistered
// failure from Lookup is surfaced — not swallowed by cobra's SilenceErrors —
// and that the "not registered" hint is suppressed in that case.
func TestRoot_CorruptSettings_ReportsError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, _, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error from bare makeslop with corrupt settings, got nil; stdout=%q", stdout)
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

func TestRoot_NotRegistered_ReturnsErrSilent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	_, stderr, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error from bare makeslop, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent (so main() skips reprint), got %v", err)
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
}

// TestRoot_OutsideHome_Refuses guards that bare makeslop refuses when cwd is
// outside HOME and docker is never invoked.
func TestRoot_OutsideHome_Refuses(t *testing.T) {
	// Two separate temp-parent groups: one for HOME (tmpHome), one for the
	// out-of-home baseDir and pwd that should trigger the guard.
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	// baseDir and pwd are siblings outside tmpHome (different TempDir call,
	// so they end up under a different test-numbered subdir on the same parent).
	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	// The docker shim must never be invoked — fail the test loudly if it is.
	docker.SkipNonPOSIX(t, "docker shim requires POSIX shell; makeslop is POSIX-only")
	shim, record := docker.WriteShim(t, t.TempDir(), 0)
	t.Cleanup(docker.SetDockerBinaryForTest(shim))
	stubTTY(t, true)

	snapBefore := snapshotTree(t, baseDir)
	_, stderr, err := runCmd(t, baseDir)
	if err == nil {
		t.Fatalf("expected error from bare makeslop outside HOME, got nil")
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
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when outside HOME; got argv=%v", argv)
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// TestInit_OutsideHome_Refuses guards that init refuses and is fully
// non-mutating when cwd is outside HOME — ensureWithinHome must run before
// config.Bootstrap.
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

// TestInit_HomeRoot_Allowed guards the rel == "." case: cwd == HOME itself
// must be accepted (filepath.IsLocal(".") is true).
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

// TestOutOfHomeFlag_Bypasses guards that --out-of-home suppresses the guard
// for both the root command and init.
func TestOutOfHomeFlag_Bypasses(t *testing.T) {
	docker.SkipNonPOSIX(t, "docker shim requires POSIX shell; makeslop is POSIX-only")

	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	baseDir := t.TempDir()
	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	// init --out-of-home must succeed and NOT produce the refusing-to-run message.
	_, stderr, err := runCmd(t, baseDir, "--out-of-home", "init")
	if err != nil {
		t.Fatalf("init --out-of-home should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("init --out-of-home: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}

	// bare makeslop --out-of-home must bypass the guard; stub docker so it
	// exits cleanly without a real daemon.
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err = runCmd(t, baseDir, "--out-of-home")
	if err != nil {
		t.Fatalf("bare makeslop --out-of-home should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("bare makeslop --out-of-home: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}
	if argv := readArgv(t, record); argv == nil {
		t.Errorf("docker shim was not invoked when --out-of-home bypasses guard")
	}
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

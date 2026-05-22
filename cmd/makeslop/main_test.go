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
	"github.com/Zwergpro/makeslop/internal/projectconfig"
	"github.com/Zwergpro/makeslop/internal/security"
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

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
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

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	if _, _, err := runCmd(t, baseDir, "go"); err != nil {
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

func TestGo_Unregistered_DoesNotInvokeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatalf("expected error from unregistered makeslop go, got nil")
	}
	if !strings.Contains(stderr, "no workspace registered") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked for unregistered workspace; got argv=%v", argv)
	}
}

// Exercises the production ttyCheck (pipes in `go test` are not TTYs).
func TestGo_NoTTY_FailsBeforeDocker(t *testing.T) {
	docker.SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	installFdShim(t, nil)
	record := installDockerShim(t, 0)
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
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when TTY check fails; got argv=%v", argv)
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
	installFdShim(t, nil)
	installDockerShim(t, 42)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
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

	installFdShim(t, nil)

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
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code != 137 {
		t.Errorf("runWithExitCode = %d, want 137 (128+SIGKILL); stderr=%q", code, stderr.String())
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

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	if _, _, err := runCmd(t, baseDir, "go"); err != nil {
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

// TestGo_CorruptSettings_ReportsError guards that a non-ErrNotRegistered
// failure from Lookup is surfaced — not swallowed by cobra's SilenceErrors —
// and that the "not registered" hint is suppressed in that case.
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

// TestGo_OutsideHome_Refuses guards that makeslop go refuses when cwd is
// outside HOME and docker is never invoked.
func TestGo_OutsideHome_Refuses(t *testing.T) {
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
// for both go and init.
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

	// makeslop --out-of-home go must bypass the guard; stub docker so it
	// exits cleanly without a real daemon.
	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err = runCmd(t, baseDir, "--out-of-home", "go")
	if err != nil {
		t.Fatalf("makeslop --out-of-home go should succeed outside HOME, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("makeslop --out-of-home go: stderr unexpectedly contains 'refusing to run': %q", stderr)
	}
	if argv := readArgv(t, record); argv == nil {
		t.Errorf("docker shim was not invoked when --out-of-home bypasses guard")
	}
}

// installFdShim writes a shim that emits the given paths null-separated and
// returns its argv record path. The shim is registered as the active fd binary
// for the duration of the test.
func installFdShim(t *testing.T, paths []string) string {
	t.Helper()
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
	shim, record := security.WriteFdShim(t, t.TempDir(), paths)
	t.Cleanup(security.SetFdBinaryForTest(shim))
	return record
}

// TestGo_FdMissing_RefusesAndDoesNotInvokeDocker verifies that when the fd
// binary is not found, makeslop prints the install hint to stderr, returns
// errSilent, and never invokes docker.
func TestGo_FdMissing_RefusesAndDoesNotInvokeDocker(t *testing.T) {
	docker.SkipNonPOSIX(t, "docker shim requires POSIX shell; makeslop is POSIX-only")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Register workspace so we get past ws.Lookup.
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Point fd to a nonexistent binary so Scan returns ErrFdMissing.
	t.Cleanup(security.SetFdBinaryForTest("/nonexistent/fd-binary-that-does-not-exist"))

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	snapBefore := snapshotTree(t, baseDir)
	_, stderr, err := runCmd(t, baseDir, "go")
	if err == nil {
		t.Fatalf("expected error when fd is missing, got nil")
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "fd/fdfind CLI required") {
		t.Errorf("stderr missing install hint: %q", stderr)
	}
	if !strings.Contains(stderr, "https://github.com/sharkdp/fd") {
		t.Errorf("stderr missing install URL: %q", stderr)
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when fd is missing; got argv=%v", argv)
	}
	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// TestGo_MasksFoundEnvFiles_ArgvContainsDevNullMounts verifies that when the
// fd shim returns two .env paths, the docker argv contains the expected
// /dev/null mounts in tail position and stderr reports the count.
func TestGo_MasksFoundEnvFiles_ArgvContainsDevNullMounts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Register workspace so we get a workspaceRoot == pwd.
	initOut, _, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	workspaceDir := strings.TrimSpace(initOut)

	resolvedPwd := evalSymlinks(t, pwd)

	// Create two .env files inside the project root so the shim paths are real files.
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

	// Shim reports both files (already in sorted order as Scan would deliver).
	installFdShim(t, []string{envFile1, envFile2})

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("root failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "makeslop: masked 2 .env file(s)") {
		t.Errorf("stderr missing masked count: %q", stderr)
	}

	name := filepath.Base(workspaceDir)
	argv := readArgv(t, record)

	// The two /dev/null overlay mounts must appear at the tail of the argv.
	wantMount1 := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	wantMount2 := "type=bind,source=/dev/null,target=/workspace/" + name + "/configs/local.env"
	var found1, found2 bool
	for _, a := range argv {
		if a == wantMount1 {
			found1 = true
		}
		if a == wantMount2 {
			found2 = true
		}
	}
	if !found1 {
		t.Errorf("argv missing /dev/null mount for .env: want %q\nargv: %v", wantMount1, argv)
	}
	if !found2 {
		t.Errorf("argv missing /dev/null mount for local.env: want %q\nargv: %v", wantMount2, argv)
	}

	// Verify the overlay mounts appear after the project bind mount (tail ordering).
	projectMount := "type=bind,source=" + resolvedPwd + ",target=/workspace/" + name
	var projectIdx, env1Idx, env2Idx int
	for i, a := range argv {
		switch a {
		case projectMount:
			projectIdx = i
		case wantMount1:
			env1Idx = i
		case wantMount2:
			env2Idx = i
		}
	}
	if env1Idx <= projectIdx {
		t.Errorf("/dev/null mount for .env (idx %d) must come after project mount (idx %d)", env1Idx, projectIdx)
	}
	if env2Idx <= projectIdx {
		t.Errorf("/dev/null mount for local.env (idx %d) must come after project mount (idx %d)", env2Idx, projectIdx)
	}
}

// TestGo_NoEnvFiles_PrintsNothingExtraOnStderr verifies that when the fd
// shim produces empty output (no .env files), makeslop does not print any
// "masked" line to stderr and the argv contains no /dev/null mounts.
func TestGo_NoEnvFiles_PrintsNothingExtraOnStderr(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Shim returns no paths.
	installFdShim(t, nil)

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("root failed: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "masked") {
		t.Errorf("stderr must not mention 'masked' when no .env files found: %q", stderr)
	}
	argv := readArgv(t, record)
	for _, a := range argv {
		if strings.Contains(a, "/dev/null") {
			t.Errorf("argv must not contain /dev/null mounts when no .env files found: %q", a)
		}
	}
}

// TestInit_DoesNotInvokeFd verifies that the init subcommand never calls
// security.Scan — a shim that exits 1 must not cause init to fail.
func TestInit_DoesNotInvokeFd(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Install an fd shim that exits 1; if init invokes it, init will fail.
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "fd-shim")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing fd shim: %v", err)
	}
	t.Cleanup(security.SetFdBinaryForTest(shimPath))

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init must succeed regardless of fd shim; got: %v; stderr=%q", err, stderr)
	}
}

// TestRoot_BareInvocation_PrintsHelp verifies that bare makeslop (no args)
// prints cobra's help to stdout, exits 0, writes nothing to stderr, and does
// not mutate any state. No workspace init, docker shim, or fd shim required.
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

// TestMergeUniqueSorted validates the mergeUniqueSorted helper: sorted union of
// two string slices with deduplication within and across slices.
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

// TestGo_DryRun_SkipsDocker verifies the "no exec" contract: with --dry-run,
// the docker shim is never invoked regardless of TTY state.
func TestGo_DryRun_SkipsDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run should succeed, got err: %v; stderr=%q", err, stderr)
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked on --dry-run; got argv=%v", argv)
	}
	if stdout == "" {
		t.Errorf("--dry-run must print to stdout; got empty")
	}
}

// TestGo_DryRun_StdoutEqualsBuildSpecShellCommand asserts exact equality
// between runCmd stdout (after trimming the trailing newline added by
// fmt.Fprintln) and docker.BuildSpec(opts).ShellCommand(). This is the single
// source of truth for the format.
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

	installFdShim(t, nil)
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
	}).ShellCommand()

	got := strings.TrimSuffix(stdout, "\n")
	if got != want {
		t.Errorf("stdout mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

// TestGo_DryRun_ShortFlag asserts that -n is a synonym for --dry-run and
// produces identical stdout.
func TestGo_DryRun_ShortFlag(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	installFdShim(t, nil)
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

// TestGo_DryRun_NoTTY_Succeeds is the TTY-bypass regression guard. The real
// ttyCheck returns false under go test (stdin/stdout are pipes). --dry-run must
// succeed because docker.Run — which is the only caller of ttyCheck — is never
// invoked.
func TestGo_DryRun_NoTTY_Succeeds(t *testing.T) {
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Scan runs before the dry-run branch, so fd must be available.
	installFdShim(t, nil)
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

// TestGo_DryRun_Unregistered_StillRefuses verifies that the workspace-lookup
// precondition fires even with --dry-run.
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

// TestGo_DryRun_OutsideHome_StillRefuses verifies that the home-dir guard
// fires even with --dry-run.
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

// TestGo_DryRun_OutOfHomeBypasses verifies that --out-of-home combined with
// --dry-run prints the command even when cwd is outside HOME.
func TestGo_DryRun_OutOfHomeBypasses(t *testing.T) {
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")

	// Use setHomeToTestParent so that the init directory is inside home.
	setHomeToTestParent(t)
	baseDir := t.TempDir()

	// Create the workspace inside home (where init is allowed).
	insidePwd := t.TempDir()
	t.Chdir(insidePwd)
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init inside home failed: %v", err)
	}

	// Now set HOME to a new temp dir that does not contain insidePwd.
	newHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, newHome))

	// insidePwd is now "outside" the new HOME. Use it as cwd for the dry-run.
	t.Chdir(insidePwd)

	installFdShim(t, nil)
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

// TestGo_DryRun_FdMissing_StillRefuses verifies that the Scan precondition
// fires even with --dry-run.
func TestGo_DryRun_FdMissing_StillRefuses(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Point fd to a nonexistent binary so Scan returns ErrFdMissing.
	t.Cleanup(security.SetFdBinaryForTest("/nonexistent/fd-binary-that-does-not-exist"))

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err == nil {
		t.Fatalf("expected error when fd is missing, got nil; stdout=%q", stdout)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "fd/fdfind CLI required") {
		t.Errorf("stderr missing fd install hint: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must be empty when fd is missing; got %q", stdout)
	}
}

// TestGo_DryRun_CorruptSettings verifies that --dry-run propagates a wrapped
// error when settings.json is corrupt. Specifically: corrupt settings.json
// causes ws.Lookup to fail (Lookup calls config.Load internally), which
// precedes the dry-run branch — errors from preconditions propagate under
// --dry-run.
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

	installFdShim(t, nil)
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

// TestGo_DryRun_MasksEnvFiles_StdoutContainsDevNullMounts verifies that when
// the fd shim returns two .env paths, the dry-run stdout contains both
// /dev/null mount lines and stderr reports the masked count.
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

	// Create two .env files so the shim paths are real files.
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

	installFdShim(t, []string{envFile1, envFile2})
	stubTTY(t, false)

	stdout, stderr, err := runCmd(t, baseDir, "go", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "makeslop: masked 2 .env file(s)") {
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

// TestGo_DryRun_FromSubdir_MountsAncestor verifies that invoking --dry-run
// from a subdirectory mounts the registered ancestor root, not pwd.
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

	installFdShim(t, nil)
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

// TestInit_DryRunFlagRejected guards that --dry-run is scoped to goCmd and not
// a persistent flag that leaks into init.
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

// TestInit_ScaffoldsProjectConfig verifies that running init creates a
// .makeslop.yaml file with the exact stub content in the project directory.
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

// TestInit_PreservesExistingProjectConfig verifies that running init on a
// directory that already has a .makeslop.yaml with user content leaves the
// file byte-for-byte unchanged.
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

// TestInit_DoesNotInvokeScan_StillHolds re-verifies the existing invariant
// that the init subcommand never calls security.Scan. The fd shim here exits
// non-zero; if init were to invoke it, init would fail. This test exists to
// confirm that wiring projectconfig.Scaffold did not accidentally introduce a
// Scan call.
func TestInit_DoesNotInvokeScan_StillHolds(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Install an fd shim that exits 1; if init invokes it, init will fail.
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "fd-shim")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing fd shim: %v", err)
	}
	t.Cleanup(security.SetFdBinaryForTest(shimPath))

	_, stderr, err := runCmd(t, baseDir, "init")
	if err != nil {
		t.Fatalf("init must succeed regardless of fd shim; got: %v; stderr=%q", err, stderr)
	}

	// Also verify the config was scaffolded successfully.
	resolvedPwd := evalSymlinks(t, pwd)
	if _, err := os.Stat(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil {
		t.Errorf("%s not created by init: %v", projectconfig.Filename, err)
	}
}

// ── projectconfig.Load wiring tests ───────────────────────────────────────────

// TestGo_LoadsYamlAndMergesMaskedFiles verifies that when fdfind returns one
// .env path and .makeslop.yaml lists an additional file under exclude.files,
// both appear as /dev/null overlay mounts in the docker argv in lex-sorted order.
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

	// Create both files on disk so security.Scan and projectconfig.Load stat-keep them.
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

	// fd shim returns only .env; the yaml file adds private/token.txt.
	installFdShim(t, []string{envFile})
	yamlContent := "exclude:\n  dirs: []\n  files: [private/token.txt]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("go failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	argv := readArgv(t, record)
	wantMount1 := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	wantMount2 := "type=bind,source=/dev/null,target=/workspace/" + name + "/private/token.txt"
	var found1, found2 bool
	for _, a := range argv {
		if a == wantMount1 {
			found1 = true
		}
		if a == wantMount2 {
			found2 = true
		}
	}
	if !found1 {
		t.Errorf("argv missing /dev/null mount for .env: want %q\nargv: %v", wantMount1, argv)
	}
	if !found2 {
		t.Errorf("argv missing /dev/null mount for private/token.txt: want %q\nargv: %v", wantMount2, argv)
	}

	// Lex-sorted order: .env < private/token.txt — check index ordering.
	var idx1, idx2 int
	for i, a := range argv {
		if a == wantMount1 {
			idx1 = i
		}
		if a == wantMount2 {
			idx2 = i
		}
	}
	if idx1 >= idx2 {
		t.Errorf("/dev/null mount for .env (idx %d) should come before private/token.txt (idx %d) in lex order", idx1, idx2)
	}
}

// TestGo_LoadsYamlMaskedDirs_TmpfsMountInArgv verifies that a directory listed
// under exclude.dirs in .makeslop.yaml results in a --mount type=tmpfs entry in
// the docker argv in tail position (after project bind mount).
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

	// fd shim returns nothing; yaml masks node_modules as a dir.
	installFdShim(t, nil)
	yamlContent := "exclude:\n  dirs: [node_modules]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("go failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	argv := readArgv(t, record)
	wantTmpfs := "type=tmpfs,target=/workspace/" + name + "/node_modules"
	projectMount := "type=bind,source=" + resolvedPwd + ",target=/workspace/" + name

	var tmpfsIdx, projectIdx int
	var foundTmpfs bool
	for i, a := range argv {
		if a == wantTmpfs {
			foundTmpfs = true
			tmpfsIdx = i
		}
		if a == projectMount {
			projectIdx = i
		}
	}
	if !foundTmpfs {
		t.Errorf("argv missing tmpfs mount: want %q\nargv: %v", wantTmpfs, argv)
	}
	if tmpfsIdx <= projectIdx {
		t.Errorf("tmpfs mount (idx %d) must come after project bind mount (idx %d)", tmpfsIdx, projectIdx)
	}
	// Also assert no source= segment in the tmpfs mount value.
	for _, a := range argv {
		if a == wantTmpfs && strings.Contains(a, "source=") {
			t.Errorf("tmpfs mount must not contain source=: %q", a)
		}
	}
}

// TestGo_YamlAbsentIsBitIdenticalArgv verifies that when no .makeslop.yaml
// exists and fdfind returns nothing, the argv is identical to the pre-YAML
// baseline (no extra mounts).
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

	// Remove the scaffolded .makeslop.yaml so Load returns a zero Excludes.
	if err := os.Remove(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove yaml: %v", err)
	}

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("go failed: %v; stderr=%q", err, stderr)
	}

	got := readArgv(t, record)
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
	}).Args()

	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestGo_YamlDedupsAgainstScan verifies that when security.Scan and
// .makeslop.yaml both report the same file, only one /dev/null overlay appears
// in the docker argv.
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

	// Create .env on disk.
	envFile := filepath.Join(resolvedPwd, ".env")
	if err := os.WriteFile(envFile, []byte("S=1"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// fd shim returns .env; yaml also lists .env in exclude.files.
	installFdShim(t, []string{envFile})
	yamlContent := "exclude:\n  dirs: []\n  files: [.env]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("go failed: %v; stderr=%q", err, stderr)
	}

	name := filepath.Base(workspaceDir)
	argv := readArgv(t, record)
	wantMount := "type=bind,source=/dev/null,target=/workspace/" + name + "/.env"
	var count int
	for _, a := range argv {
		if a == wantMount {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 /dev/null mount for .env, got %d\nargv: %v", count, argv)
	}
}

// ── YAML error propagation tests ──────────────────────────────────────────────

// TestGo_YamlMalformedAbortsBeforeDocker verifies that a malformed .makeslop.yaml
// causes makeslop go to exit non-zero with "makeslop: " on stderr and that docker
// is never invoked (preserving the secret-masking invariant: no container starts
// means no .env leak is possible).
//
// Uses runWithExitCode (not runCmd) so that non-errSilent errors are formatted
// with the "makeslop: " prefix on stderr, matching the production code path.
func TestGo_YamlMalformedAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// Write invalid YAML.
	badYAML := []byte("exclude:\n  dirs: [unclosed\n")
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), badYAML, 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from malformed yaml, got 0; stderr=%q", stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "makeslop: ") {
		t.Errorf("stderr missing 'makeslop: ' prefix: %q", stderr.String())
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked on yaml parse error; got argv=%v", argv)
	}
}

// TestGo_YamlReservedPathAbortsBeforeDocker verifies that listing a reserved
// agent path (.claude) in exclude.dirs causes makeslop go to abort before docker
// is invoked, with an error mentioning "reserved agent path".
//
// Uses runWithExitCode so that the error message appears on stderr.
func TestGo_YamlReservedPathAbortsBeforeDocker(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// List a reserved agent path in exclude.dirs.
	yamlContent := "exclude:\n  dirs: [.claude]\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from reserved path, got 0; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reserved agent path") {
		t.Errorf("stderr missing 'reserved agent path': %q", stderr.String())
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when yaml lists reserved path; got argv=%v", argv)
	}
}

// TestGo_YamlDirAndFileDupAborts verifies that listing the same relative path in
// both exclude.dirs and exclude.files causes makeslop go to abort before docker
// is invoked, with an error mentioning "listed in both".
//
// Uses runWithExitCode so that the error message appears on stderr.
func TestGo_YamlDirAndFileDupAborts(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// Same path in both dirs and files — cross-list duplicate.
	yamlContent := "exclude:\n  dirs: [data]\n  files: [data]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"go"})
	if code == 0 {
		t.Fatalf("expected non-zero exit from cross-list dup, got 0; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "listed in both") {
		t.Errorf("stderr missing 'listed in both': %q", stderr.String())
	}
	if argv := readArgv(t, record); argv != nil {
		t.Errorf("docker shim must not be invoked when yaml has cross-list dup; got argv=%v", argv)
	}
}

// TestGo_YamlMissingPathSkippedSilently verifies that when .makeslop.yaml lists
// a file that does not exist on disk, makeslop go exits 0, produces no error
// output mentioning the missing path, and the docker argv contains no /dev/null
// overlay for that path (the silent-skip flows through to the cobra layer).
func TestGo_YamlMissingPathSkippedSilently(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// List a path that does NOT exist on disk.
	yamlContent := "exclude:\n  dirs: []\n  files: [secrets/api.key]\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	// Explicitly ensure the file does not exist.
	_ = os.Remove(filepath.Join(resolvedPwd, "secrets", "api.key"))

	installFdShim(t, nil)
	record := installDockerShim(t, 0)
	stubTTY(t, true)

	_, stderr, err := runCmd(t, baseDir, "go")
	if err != nil {
		t.Fatalf("expected success when missing path is silently skipped, got: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "api.key") {
		t.Errorf("stderr must not mention missing path 'api.key': %q", stderr)
	}

	// The docker argv must not contain any /dev/null overlay for secrets/api.key.
	argv := readArgv(t, record)
	for _, a := range argv {
		if strings.Contains(a, "api.key") {
			t.Errorf("argv must not contain overlay for missing api.key: %q", a)
		}
	}
}

// TestGo_DryRunIncludesMaskedDirs verifies that --dry-run stdout includes the
// tmpfs mount for a directory listed in .makeslop.yaml exclude.dirs.
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

	// Create the directory on disk.
	secretsDir := filepath.Join(resolvedPwd, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}

	// fd shim returns nothing; yaml masks secrets/ as a dir.
	installFdShim(t, nil)
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
	// Verify no source= in the tmpfs mount line.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "tmpfs") && strings.Contains(line, "source=") {
			t.Errorf("tmpfs mount line must not contain source=: %q", line)
		}
	}
}

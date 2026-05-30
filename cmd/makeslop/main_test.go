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

	"github.com/Zwergpro/makeslop/internal/assets"
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
	docker.SkipNonPOSIX(t, "docker shim requires POSIX shell; makeslop is POSIX-only")

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

// installFdShim installs a shim that emits the given paths null-separated;
// active for the test duration via SetFdBinaryForTest.
func installFdShim(t *testing.T, paths []string) string {
	t.Helper()
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
	shim, record := security.WriteFdShim(t, t.TempDir(), paths)
	t.Cleanup(security.SetFdBinaryForTest(shim))
	return record
}

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

func TestGo_NoEnvFiles_PrintsNothingExtraOnStderr(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

// TTY-bypass guard: --dry-run succeeds even when real ttyCheck returns false
// because docker.Run (the only ttyCheck caller) is never invoked.
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
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")

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

func TestGo_DryRun_FdMissing_StillRefuses(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

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

// Re-verifies the init-does-not-invoke-Scan invariant after Scaffold wiring.
// If init calls the shim (which exits 1), init will fail.
func TestInit_DoesNotInvokeScan_StillHolds(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

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

	resolvedPwd := evalSymlinks(t, pwd)
	if _, err := os.Stat(filepath.Join(resolvedPwd, projectconfig.Filename)); err != nil {
		t.Errorf("%s not created by init: %v", projectconfig.Filename, err)
	}
}

// ── projectconfig.Load wiring tests ───────────────────────────────────────────

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
	for _, a := range argv {
		if a == wantTmpfs && strings.Contains(a, "source=") {
			t.Errorf("tmpfs mount must not contain source=: %q", a)
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

	argv := readArgv(t, record)
	for _, a := range argv {
		if strings.Contains(a, "api.key") {
			t.Errorf("argv must not contain overlay for missing api.key: %q", a)
		}
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

	installFdShim(t, nil)
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
	docker.SkipNonPOSIX(t, "fd shim requires POSIX shell; makeslop is POSIX-only")
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

	installFdShim(t, nil)
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
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "tmpfs") && strings.Contains(line, "source=") {
			t.Errorf("tmpfs mount line must not contain source=: %q", line)
		}
	}
}

// ── build subcommand tests ─────────────────────────────────────────────────────

// executableTempDirForCmd creates a temp dir under /workspace (which is
// executable, unlike /tmp in this environment) for use with build shims.
// The dir is registered for cleanup via t.Cleanup.
func executableTempDirForCmd(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/workspace", "makeslop-cmd-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp /workspace: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
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

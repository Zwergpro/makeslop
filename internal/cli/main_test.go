package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
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

func (f *fakeDocker) Build(_ context.Context, o docker.BuildOptions, _ io.Writer) error {
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
	return exitCodeFromError(cmd.ExecuteContext(context.Background()), stderr)
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
	return mapKeys(snapshotTree(t, root))
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

// ── Root bare-invocation tests (cross-cutting: all commands) ──────────────────

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

// ── RunWithExitCode tests ─────────────────────────────────────────────────────

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

// ── Quiet tests (cross-cutting) ───────────────────────────────────────────────

// --quiet suppresses the stale-config nudge (chrome) but not errors.
func TestQuiet_SuppressesInitNudge(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Version: 0 forces staleness since 0 < ConfigVersion(1).
	s := &config.Settings{
		Version:    0, // stale
		Image:      config.DefaultImage,
		Shell:      config.DefaultShell,
		TmpDirSize: config.DefaultTmpDirSize,
		Workspaces: map[string]config.Workspace{},
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

	s.Version = 0 // re-seed stale for the next call
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

// ── Error-voice tests (cross-cutting) ────────────────────────────────────────

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

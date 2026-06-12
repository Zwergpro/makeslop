package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func newFakeStatusDeps(daemonDown bool, imageMissing bool) dockerDeps {
	fc := newFakeDocker(0, false) // TTY irrelevant for status
	if daemonDown {
		fc.PingErr = errors.New("connection refused")
	}
	fc.ImageMissing = imageMissing
	return depsFrom(fc)
}

func newFakeStatusDepsWithImageErr(imageErr error) dockerDeps {
	fc := newFakeDocker(0, false)
	fc.ImageErr = imageErr
	return depsFrom(fc)
}

// All checks pass → exit 0, ready.
func TestStatus_AllGreen_ExitsZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err != nil {
		t.Errorf("status should exit 0 when all checks pass; err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "ready") {
		t.Errorf("stderr missing 'ready' verdict: %q", stderr)
	}
	if strings.Contains(stderr, "not ready") {
		t.Errorf("all-green status must not contain 'not ready': %q", stderr)
	}
}

// Daemon down → exit non-zero, daemon failure reported.
func TestStatus_DaemonDown_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(true, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero when daemon is down; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Errorf("stderr missing 'not ready': %q", stderr)
	}
	if !strings.Contains(stderr, "daemon") {
		t.Errorf("stderr missing 'daemon' check: %q", stderr)
	}
}

// Missing image → exit non-zero, build hint.
func TestStatus_ImageMissing_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(false, true)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero when image is missing; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Errorf("stderr missing 'not ready': %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop build") {
		t.Errorf("stderr missing 'makeslop build' hint: %q", stderr)
	}
}

// ImageExists returns an error (not just "missing") → exit non-zero, error detail shown.
func TestStatus_ImageCheckError_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDepsWithImageErr(errors.New("transport error: dial tcp"))

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero when image check errors; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Errorf("stderr missing 'not ready': %q", stderr)
	}
	if !strings.Contains(stderr, "error checking image") {
		t.Errorf("stderr missing 'error checking image' detail: %q", stderr)
	}
}

// Unregistered workspace → exit non-zero, init hint.
func TestStatus_WorkspaceNotRegistered_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd) // no init

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero when workspace not registered; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Errorf("stderr missing 'not ready': %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop init") {
		t.Errorf("stderr missing 'makeslop init' hint: %q", stderr)
	}
}

// A stale base config is non-blocking: warn line, but verdict stays ready.
func TestStatus_StaleConfig_ReportsWarnButStaysReady(t *testing.T) {
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
	// Force staleness: Version 0 < ConfigVersion(1).
	s.Version = 0
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("save stale settings: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	_, stderr, statusErr := runCmdWithDeps(t, baseDir, deps, "status")
	if statusErr != nil {
		t.Errorf("status must be ready despite stale config; err=%v stderr=%q", statusErr, stderr)
	}
	if !strings.Contains(stderr, "ready") {
		t.Errorf("stderr missing 'ready' verdict: %q", stderr)
	}
	if strings.Contains(stderr, "not ready") {
		t.Errorf("stale-config status must not contain 'not ready' (stale is non-blocking): %q", stderr)
	}
	if !strings.Contains(stderr, "base config") {
		t.Errorf("stderr missing 'base config' check: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop migrate") {
		t.Errorf("stderr missing 'makeslop migrate' hint in base config line: %q", stderr)
	}
}

// --json emits {checks:[{name,state,detail}], ready:bool}.
func TestStatus_JSON_Shape(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	stdout, stderr, cmdErr := runCmdWithDeps(t, baseDir, deps, "status", "--json")
	if cmdErr != nil {
		t.Fatalf("status --json failed unexpectedly: %v; stderr=%q", cmdErr, stderr)
	}

	if stdout == "" {
		t.Fatal("--json output is empty")
	}

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	const wantChecks = 5 // daemon, base config, image, workspace, secret scan
	if len(result.Checks) != wantChecks {
		t.Errorf("--json result.checks len = %d, want %d (daemon, base config, image, workspace, secret scan)",
			len(result.Checks), wantChecks)
	}
	validStates := map[checkState]bool{
		checkOK:   true,
		checkFail: true,
		checkWarn: true,
		checkInfo: true,
	}
	for _, c := range result.Checks {
		if c.Name == "" {
			t.Errorf("check with empty name: %+v", c)
		}
		if !validStates[c.State] {
			t.Errorf("check %q has invalid state %q", c.Name, c.State)
		}
	}
}

// --json ready is false when a blocking check (daemon) fails.
func TestStatus_JSON_ReadyField(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(true, false)

	// status exits non-zero when a blocking check fails; JSON is still written to stdout.
	stdout, _, _ := runCmdWithDeps(t, baseDir, deps, "status", "--json")

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if result.Ready {
		t.Errorf("--json ready must be false when daemon is down; got true")
	}

	var daemonCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "daemon" {
			daemonCheck = &result.Checks[i]
			break
		}
	}
	if daemonCheck == nil {
		t.Fatalf("--json missing 'daemon' check; got: %+v", result.Checks)
	}
	if daemonCheck.State != checkFail {
		t.Errorf("daemon check state = %q, want %q", daemonCheck.State, checkFail)
	}
}

// Plain (non-TTY) output uses ASCII bracket glyphs, not Unicode. Exercises
// renderChecks directly to avoid PTY coupling.
func TestStatus_PlainOutput_NoGlyphs(t *testing.T) {
	var buf bytes.Buffer
	checks := []statusCheck{
		{Name: "daemon", State: checkOK},
		{Name: "image", State: checkFail, Detail: "image not found"},
		{Name: "workspace", State: checkWarn, Detail: "stale"},
		{Name: "secret scan", State: checkInfo},
	}
	renderChecks(&buf, checks, false, false /* tty=false */)

	out := buf.String()

	if !strings.Contains(out, "[ok]") {
		t.Errorf("plain output missing '[ok]': %q", out)
	}
	if !strings.Contains(out, "[fail]") {
		t.Errorf("plain output missing '[fail]': %q", out)
	}
	if !strings.Contains(out, "[!]") {
		t.Errorf("plain output missing '[!]': %q", out)
	}
	if !strings.Contains(out, "[–]") {
		t.Errorf("plain output missing '[–]': %q", out)
	}

	if strings.Contains(out, "✓") {
		t.Errorf("plain output must not contain '✓': %q", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("plain output must not contain '✗': %q", out)
	}

	if !strings.Contains(out, "not ready") {
		t.Errorf("plain output missing 'not ready' verdict: %q", out)
	}
}

// TTY mode uses Unicode glyphs.
func TestStatus_TTYOutput_UsesGlyphs(t *testing.T) {
	var buf bytes.Buffer
	checks := []statusCheck{
		{Name: "daemon", State: checkOK},
		{Name: "image", State: checkOK},
	}
	renderChecks(&buf, checks, true, true /* tty=true */)

	out := buf.String()

	if !strings.Contains(out, "✓") {
		t.Errorf("TTY output missing '✓': %q", out)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("TTY output missing 'ready' verdict: %q", out)
	}
	if strings.Contains(out, "not ready") {
		t.Errorf("all-green TTY output must not contain 'not ready': %q", out)
	}
}

// status is exempt from the home-dir guard.
func TestStatus_ExemptFromHomeGuard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	baseDir := t.TempDir()

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	// May fail for daemon/image/workspace reasons, but never home-dir.
	if err != nil && strings.Contains(stderr, "refusing to run from") {
		t.Errorf("status must not apply the home-dir guard; stderr=%q", stderr)
	}
}

// status is CI/pipe-safe: no TTY required.
func TestStatus_ExemptFromTTYRequirement(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err != nil {
		// Any failure is fine except a TTY-related one.
		if strings.Contains(stderr, "TTY") || strings.Contains(stderr, "tty") {
			t.Errorf("status must not require a TTY; err=%v stderr=%q", err, stderr)
		}
	}
}

func TestStatus_ListedInHelp(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir)
	if err != nil {
		t.Fatalf("bare makeslop should exit 0; err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "\n  status ") {
		t.Errorf("stdout missing '\\n  status ' command entry: %q", stdout)
	}
}

// A projectconfig.Load failure (check 5) is non-blocking: warn, status stays ready.
func TestStatus_Check5_PCErrShowsWarn(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	// Stale network: block that projectconfig.Load rejects.
	staleYAML := "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: 10.0.0.5:3128\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(staleYAML), 0o644); err != nil {
		t.Fatalf("write stale yaml: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err != nil {
		t.Errorf("status must remain ready despite pcErr (non-blocking); err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "secret scan") {
		t.Errorf("stderr missing 'secret scan' check line: %q", stderr)
	}
	if !strings.Contains(stderr, ".makeslop.yaml") {
		t.Errorf("stderr missing .makeslop.yaml reference in scan warn: %q", stderr)
	}
}

// A security.Scan error (check 5) is non-blocking: warn, status stays ready.
// Induced by an unreadable subdir that fails WalkDir.
func TestStatus_Check5_ScanErrShowsWarn(t *testing.T) {
	skipNonPOSIX(t, "chmod 0000 requires POSIX")
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	unreadable := filepath.Join(resolvedPwd, "secrets")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A scan pattern so Scan actually walks.
	yamlContent := "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n    skip-dirs: []\n  dirs: []\n  files: []\n"
	if err := os.WriteFile(filepath.Join(resolvedPwd, projectconfig.Filename), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err != nil {
		t.Errorf("scan error must be non-blocking (warn), not blocking; err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "scan error") {
		t.Errorf("stderr missing 'scan error' indicator for failed scan; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "secret scan") {
		t.Errorf("stderr missing 'secret scan' check name; stderr=%q", stderr)
	}
}

// When the scan finds masked files, check 5 shows OK with "will mask N file(s)".
func TestStatus_Check5_MaskedFilesShowsOKWithCount(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	resolvedPwd := evalSymlinks(t, pwd)

	secretFile := filepath.Join(resolvedPwd, ".env")
	if err := os.WriteFile(secretFile, []byte("SECRET=val"), 0o644); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err != nil {
		t.Errorf("status must be ready when masked files found; err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "will mask") {
		t.Errorf("stderr missing 'will mask' detail: %q", stderr)
	}
	if !strings.Contains(stderr, "file(s)") {
		t.Errorf("stderr missing 'file(s)' in mask count: %q", stderr)
	}
}

func TestStatus_RenderReadyVerdict(t *testing.T) {
	var buf bytes.Buffer
	checks := []statusCheck{
		{Name: "daemon", State: checkOK},
		{Name: "image", State: checkOK},
	}
	renderChecks(&buf, checks, true, false)

	out := buf.String()
	if !strings.Contains(out, "ready") {
		t.Errorf("ready verdict missing: %q", out)
	}
	if strings.Contains(out, "not ready") {
		t.Errorf("ready path must not contain 'not ready': %q", out)
	}
}

// Corrupt settings → base-config check fails; workspace check shows
// "cannot check — settings unreadable" rather than a redundant parse error.
func TestStatus_CorruptSettings_WorkspaceShowsCannotCheck(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Write corrupt settings (no prior init).
	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt settings: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	_, stderr, err := runCmdWithDeps(t, baseDir, deps, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero with corrupt settings; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "cannot check") {
		t.Errorf("workspace check must show 'cannot check' when settings are unreadable: %q", stderr)
	}
	// Ensure the base-config check (not workspace) shows the parse error.
	if !strings.Contains(stderr, "base config") {
		t.Errorf("stderr must mention 'base config' check: %q", stderr)
	}
}

// TestStatus_CheckOrdering verifies that the five status checks appear in
// the documented order: daemon → base config → image → workspace → secret scan.
// Reordering the checks in runStatus would break the first-failing-check remedy
// logic and CI integrations that parse the JSON by index.
func TestStatus_CheckOrdering(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	deps := newFakeStatusDeps(false, false)

	stdout, stderr, cmdErr := runCmdWithDeps(t, baseDir, deps, "status", "--json")
	if cmdErr != nil {
		t.Fatalf("status --json failed unexpectedly: %v; stderr=%q", cmdErr, stderr)
	}

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output not valid JSON: %v\noutput: %s", err, stdout)
	}

	wantOrder := []string{"daemon", "base config", "image", "workspace", "secret scan"}
	if len(result.Checks) != len(wantOrder) {
		t.Fatalf("want %d checks, got %d: %v", len(wantOrder), len(result.Checks), result.Checks)
	}
	for i, want := range wantOrder {
		if result.Checks[i].Name != want {
			t.Errorf("check[%d].Name = %q, want %q (full order: %v)",
				i, result.Checks[i].Name, want, result.Checks)
		}
	}
}

// ── checkList unit tests ──────────────────────────────────────────────────────

// ok appends an ok check; ready stays true.
func TestCheckList_Ok_AppendsAndStaysReady(t *testing.T) {
	cl := &checkList{}
	cl.ok("daemon", "")
	if !cl.ready() {
		t.Errorf("ok() must not clear ready")
	}
	if len(cl.checks) != 1 {
		t.Fatalf("want 1 check, got %d", len(cl.checks))
	}
	if cl.checks[0].State != checkOK {
		t.Errorf("state = %q, want %q", cl.checks[0].State, checkOK)
	}
	if cl.checks[0].Name != "daemon" {
		t.Errorf("name = %q, want %q", cl.checks[0].Name, "daemon")
	}
}

// fail appends a fail check and clears ready.
func TestCheckList_Fail_ClearsReady(t *testing.T) {
	cl := &checkList{}
	cl.ok("daemon", "")
	cl.fail("image", "run 'makeslop build'")
	if cl.ready() {
		t.Errorf("fail() must clear ready")
	}
	if len(cl.checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(cl.checks))
	}
	if cl.checks[1].State != checkFail {
		t.Errorf("state = %q, want %q", cl.checks[1].State, checkFail)
	}
	if cl.checks[1].Detail != "run 'makeslop build'" {
		t.Errorf("detail = %q, want %q", cl.checks[1].Detail, "run 'makeslop build'")
	}
}

// warn appends a warn check but does NOT clear ready.
func TestCheckList_Warn_NonBlocking(t *testing.T) {
	cl := &checkList{}
	cl.warn("base config", "stale — run migrate")
	if !cl.ready() {
		t.Errorf("warn() must not clear ready")
	}
	if cl.checks[0].State != checkWarn {
		t.Errorf("state = %q, want %q", cl.checks[0].State, checkWarn)
	}
}

// info appends an info check; ready unchanged.
func TestCheckList_Info_NonBlocking(t *testing.T) {
	cl := &checkList{}
	cl.info("secret scan")
	if !cl.ready() {
		t.Errorf("info() must not clear ready")
	}
	if cl.checks[0].State != checkInfo {
		t.Errorf("state = %q, want %q", cl.checks[0].State, checkInfo)
	}
	if cl.checks[0].Detail != "" {
		t.Errorf("info() must not set Detail, got %q", cl.checks[0].Detail)
	}
}

// ready is cleared by the first fail and stays false across subsequent calls.
func TestCheckList_ReadyClearedByFirstFail(t *testing.T) {
	cl := &checkList{}
	cl.fail("daemon", "down")
	cl.ok("image", "")   // subsequent ok does not restore ready
	cl.warn("scan", "x") // subsequent warn does not restore ready
	if cl.ready() {
		t.Errorf("ready must stay false after fail()")
	}
}

// The "not ready" verdict names the first failing check's remedy only.
func TestStatus_RenderNotReadyVerdict(t *testing.T) {
	var buf bytes.Buffer
	checks := []statusCheck{
		{Name: "daemon", State: checkFail, Detail: "run 'docker info'"},
		{Name: "image", State: checkFail, Detail: "run 'makeslop build'"},
	}
	renderChecks(&buf, checks, false, false)

	out := buf.String()
	if !strings.Contains(out, "not ready") {
		t.Errorf("verdict missing 'not ready': %q", out)
	}
	if !strings.Contains(out, "run 'docker info'") {
		t.Errorf("verdict must name first failing check remedy: %q", out)
	}
	// The second check's remedy may appear in its detail line but not the verdict line.
	if strings.Contains(out, "run 'makeslop build'") {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		lastLine := lines[len(lines)-1]
		if strings.Contains(lastLine, "run 'makeslop build'") {
			t.Errorf("verdict line must only mention first failing check remedy; got: %q", lastLine)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
)

// runStatusCmd calls the status command and returns stdout, stderr, and the
// cobra-layer error. The isTTY predicate in the production newStatusCmd is
// wired to defaultIsTTY, which returns false for non-file writers — so tests
// naturally get plain (no-glyph) output via the bytes.Buffer stderr sink.
func runStatusCmd(t *testing.T, baseDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCmd(t, baseDir, args...)
}

// installFakeStatusClient installs a fake docker client with scripted
// daemon/image state for status tests. The fake is torn down via t.Cleanup.
func installFakeStatusClient(t *testing.T, daemonDown bool, imageMissing bool) *docker.FakeRunClient {
	t.Helper()
	return installFakeStatusClientFull(t, daemonDown, imageMissing, false)
}

// installFakeStatusClientFull is the full-control variant of
// installFakeStatusClient. socatMissing controls whether the socat image
// inspect returns not-found (simulating alpine/socat not yet pulled).
func installFakeStatusClientFull(t *testing.T, daemonDown bool, imageMissing bool, socatMissing bool) *docker.FakeRunClient {
	t.Helper()
	fc := docker.NewFakeRunClient(0)
	if daemonDown {
		fc.PingErr = errors.New("connection refused")
	}
	fc.ImageMissing = imageMissing
	fc.SocatImageMissing = socatMissing
	t.Cleanup(docker.SetClientForTest(fc))
	return fc
}

// TestStatus_AllGreen_ExitsZero verifies that when daemon is up, image exists,
// and workspace is registered, `status` exits 0 and reports ready.
func TestStatus_AllGreen_ExitsZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Init registers the workspace.
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon up, image present.
	installFakeStatusClient(t, false, false)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
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

// TestStatus_DaemonDown_ExitsNonZero verifies that a daemon-down state causes
// status to exit non-zero and report the daemon failure.
func TestStatus_DaemonDown_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon down.
	installFakeStatusClient(t, true, false)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
	if err == nil {
		t.Fatalf("status should exit non-zero when daemon is down; stderr=%q", stderr)
	}
	if !errors.Is(err, errSilent) {
		t.Errorf("expected errSilent, got %v", err)
	}
	if !strings.Contains(stderr, "not ready") {
		t.Errorf("stderr missing 'not ready': %q", stderr)
	}
	// The daemon failure line must be present.
	if !strings.Contains(stderr, "daemon") {
		t.Errorf("stderr missing 'daemon' check: %q", stderr)
	}
}

// TestStatus_ImageMissing_ExitsNonZero verifies that a missing image causes
// status to exit non-zero and report the image failure with a build hint.
func TestStatus_ImageMissing_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon up, image missing.
	installFakeStatusClient(t, false, true)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
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

// TestStatus_WorkspaceNotRegistered_ExitsNonZero verifies that an unregistered
// workspace causes status to exit non-zero with an init hint.
func TestStatus_WorkspaceNotRegistered_ExitsNonZero(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)
	// No init — workspace not registered.

	installFakeStatusClient(t, false, false)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
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

// TestStatus_StaleConfig_ReportsWarnButStaysReady verifies that a stale base
// config emits a warn line but still produces a ready verdict when all
// blocking checks pass.
func TestStatus_StaleConfig_ReportsWarnButStaysReady(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if config.MigrationVersion == 0 {
		t.Skip("MigrationVersion is 0; cannot test stale config")
	}

	// First init seeds at MigrationVersion, then reset MigratedVersion to 0.
	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	s.MigratedVersion = 0
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("save stale settings: %v", err)
	}

	installFakeStatusClient(t, false, false)

	_, stderr, statusErr := runStatusCmd(t, baseDir, "status")
	if statusErr != nil {
		t.Errorf("status must be ready despite stale config; err=%v stderr=%q", statusErr, stderr)
	}
	if !strings.Contains(stderr, "ready") {
		t.Errorf("stderr missing 'ready' verdict: %q", stderr)
	}
	if strings.Contains(stderr, "not ready") {
		t.Errorf("stale-config status must not contain 'not ready' (stale is non-blocking): %q", stderr)
	}
	// The base-config line must show the stale state.
	if !strings.Contains(stderr, "base config") {
		t.Errorf("stderr missing 'base config' check: %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop migrate") {
		t.Errorf("stderr missing 'makeslop migrate' hint in base config line: %q", stderr)
	}
}

// TestStatus_JSON_Shape verifies that --json emits valid JSON with the expected
// top-level shape: {checks:[{name,state,detail}], ready:bool}.
func TestStatus_JSON_Shape(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	installFakeStatusClient(t, false, false)

	stdout, _, _ := runStatusCmd(t, baseDir, "status", "--json")

	if stdout == "" {
		t.Fatal("--json output is empty")
	}

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if len(result.Checks) == 0 {
		t.Errorf("--json result.checks is empty")
	}
	// Verify each check has a non-empty name and a valid state.
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

// TestStatus_JSON_ReadyField verifies that the ready field in --json output
// is false when a blocking check fails (daemon down).
func TestStatus_JSON_ReadyField(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon down — ready must be false.
	installFakeStatusClient(t, true, false)

	stdout, _, _ := runStatusCmd(t, baseDir, "status", "--json")

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}
	if result.Ready {
		t.Errorf("--json ready must be false when daemon is down; got true")
	}

	// Verify daemon check is present with fail state.
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

// TestStatus_PlainOutput_NoGlyphs verifies that plain (non-TTY) output uses
// ASCII bracket glyphs ([ok], [fail], etc.) rather than Unicode symbols.
// This exercises renderChecks directly so no cobra/PTY coupling is needed.
func TestStatus_PlainOutput_NoGlyphs(t *testing.T) {
	var buf bytes.Buffer
	checks := []statusCheck{
		{Name: "daemon", State: checkOK},
		{Name: "image", State: checkFail, Detail: "image not found"},
		{Name: "workspace", State: checkWarn, Detail: "stale"},
		{Name: "proxy", State: checkInfo},
	}
	renderChecks(&buf, checks, false, false /* tty=false */)

	out := buf.String()

	// Plain mode must use bracket glyphs.
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

	// Must not contain Unicode checkmark glyphs.
	if strings.Contains(out, "✓") {
		t.Errorf("plain output must not contain '✓': %q", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("plain output must not contain '✗': %q", out)
	}

	// Verdict line must say "not ready" (image check failed).
	if !strings.Contains(out, "not ready") {
		t.Errorf("plain output missing 'not ready' verdict: %q", out)
	}
}

// TestStatus_TTYOutput_UsesGlyphs verifies that TTY mode uses Unicode glyphs.
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

// TestStatus_ExemptFromHomeGuard verifies that `status` does not trigger the
// home-dir guard when cwd is outside HOME.
func TestStatus_ExemptFromHomeGuard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	baseDir := t.TempDir()

	installFakeStatusClient(t, false, false)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
	// Status may fail for daemon/image/workspace reasons but NOT for home-dir.
	if err != nil && strings.Contains(stderr, "refusing to run from") {
		t.Errorf("status must not apply the home-dir guard; stderr=%q", stderr)
	}
}

// TestStatus_ExemptFromTTYRequirement verifies that `status` runs even when
// stdin/stdout are not TTYs (CI/pipe-safe).
func TestStatus_ExemptFromTTYRequirement(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	installFakeStatusClient(t, false, false)
	// Do NOT stub ttyCheck — real predicate returns false in go test.

	_, stderr, err := runStatusCmd(t, baseDir, "status")
	if err != nil {
		// Any failure is acceptable here *except* a TTY-related one.
		if strings.Contains(stderr, "TTY") || strings.Contains(stderr, "tty") {
			t.Errorf("status must not require a TTY; err=%v stderr=%q", err, stderr)
		}
	}
}

// TestStatus_ListedInHelp verifies that `status` appears in the Available
// Commands section of the bare `makeslop` help output.
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

// TestStatus_RenderReadyVerdict verifies the ready-path verdict line.
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

// TestStatus_RenderNotReadyVerdict verifies that the first failing check's
// remedy appears in the "not ready" verdict line.
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
	// Must contain the remedy from the FIRST failing check.
	if !strings.Contains(out, "run 'docker info'") {
		t.Errorf("verdict must name first failing check remedy: %q", out)
	}
	// Second check's remedy must not appear in the verdict line.
	if strings.Contains(out, "run 'makeslop build'") {
		// It's OK if the check detail line mentions it, but the verdict should only have the first.
		// Check that the remedy isn't duplicated in the verdict area.
		lines := strings.Split(strings.TrimSpace(out), "\n")
		lastLine := lines[len(lines)-1]
		if strings.Contains(lastLine, "run 'makeslop build'") {
			t.Errorf("verdict line must only mention first failing check remedy; got: %q", lastLine)
		}
	}
}

// TestStatus_ProxyLine_GatewayDefault verifies that when no proxy address is
// configured, the proxy check shows "gateway (direct egress)".
func TestStatus_ProxyLine_GatewayDefault(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	installFakeStatusClient(t, false, false)

	_, stderr, _ := runStatusCmd(t, baseDir, "status")

	if !strings.Contains(stderr, "gateway (direct egress)") {
		t.Errorf("proxy line must show 'gateway (direct egress)' when no address configured; stderr=%q", stderr)
	}
}

// TestStatus_ProxyLine_UpstreamAddress verifies that when a proxy address is
// configured, the proxy check shows the address.
func TestStatus_ProxyLine_UpstreamAddress(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Write a .makeslop.yaml with a proxy address set.
	yamlContent := `exclude:
  scan:
    patterns: []
    skip-dirs: []
  files: []
  dirs: []
network:
  proxy:
    address: "proxy.example.com:8080"
  log: ""
`
	if err := os.WriteFile(filepath.Join(pwd, ".makeslop.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	installFakeStatusClient(t, false, false)

	_, stderr, _ := runStatusCmd(t, baseDir, "status")

	if !strings.Contains(stderr, "proxy.example.com:8080") {
		t.Errorf("proxy line must show address when configured; stderr=%q", stderr)
	}
	if strings.Contains(stderr, "gateway (direct egress)") {
		t.Errorf("proxy line must not show 'gateway (direct egress)' when address is set; stderr=%q", stderr)
	}
}

// TestStatus_ProxyLine_LoggingSuffix verifies that when network.log is set,
// the logging path is appended to the proxy detail for both gateway and upstream modes.
func TestStatus_ProxyLine_LoggingSuffix(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Gateway mode (no address) with log path set.
	yamlContent := `exclude:
  scan:
    patterns: []
    skip-dirs: []
  files: []
  dirs: []
network:
  proxy:
    address: ""
  log: "makeslop-requests.log"
`
	if err := os.WriteFile(filepath.Join(pwd, ".makeslop.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	installFakeStatusClient(t, false, false)

	_, stderr, _ := runStatusCmd(t, baseDir, "status")

	if !strings.Contains(stderr, "gateway (direct egress)") {
		t.Errorf("proxy line must show 'gateway (direct egress)'; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "logging →") {
		t.Errorf("proxy line must contain 'logging →' suffix when log path is set; stderr=%q", stderr)
	}
	// Verify the full resolved absolute path appears, not just the basename.
	// status derives LogPath from projectconfig.Load which resolves it under
	// the workspace root.
	resolvedPwd := evalSymlinks(t, pwd)
	wantLogPath := filepath.Join(resolvedPwd, "makeslop-requests.log")
	if !strings.Contains(stderr, wantLogPath) {
		t.Errorf("proxy line must contain the full resolved log path %q; stderr=%q", wantLogPath, stderr)
	}
}

// TestStatus_ProxyLine_UpstreamWithLogging verifies the logging suffix appears
// when both an upstream address and a log path are configured.
func TestStatus_ProxyLine_UpstreamWithLogging(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	yamlContent := `exclude:
  scan:
    patterns: []
    skip-dirs: []
  files: []
  dirs: []
network:
  proxy:
    address: "corp.proxy:3128"
  log: "proxy.log"
`
	if err := os.WriteFile(filepath.Join(pwd, ".makeslop.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	installFakeStatusClient(t, false, false)

	_, stderr, _ := runStatusCmd(t, baseDir, "status")

	if !strings.Contains(stderr, "corp.proxy:3128") {
		t.Errorf("proxy line must show upstream address; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "logging →") {
		t.Errorf("proxy line must show logging suffix; stderr=%q", stderr)
	}
	// Verify the resolved log path appears after the arrow, not just the arrow itself.
	resolvedPwd := evalSymlinks(t, pwd)
	wantLogPath := filepath.Join(resolvedPwd, "proxy.log")
	if !strings.Contains(stderr, wantLogPath) {
		t.Errorf("proxy line must contain resolved log path %q after 'logging →'; stderr=%q", wantLogPath, stderr)
	}
}

// TestStatus_JSON_ProxyDetail_Gateway verifies that --json proxy detail shows
// "gateway (direct egress)" when no address is configured.
func TestStatus_JSON_ProxyDetail_Gateway(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	installFakeStatusClient(t, false, false)

	stdout, _, _ := runStatusCmd(t, baseDir, "status", "--json")

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}

	var proxyCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "proxy" {
			proxyCheck = &result.Checks[i]
			break
		}
	}
	if proxyCheck == nil {
		t.Fatalf("--json missing 'proxy' check; checks: %+v", result.Checks)
	}
	if proxyCheck.Detail != "gateway (direct egress)" {
		t.Errorf("proxy detail = %q, want %q", proxyCheck.Detail, "gateway (direct egress)")
	}
}

// TestStatus_JSON_ProxyDetail_LoggingSuffix verifies that when network.log is
// set, the JSON detail field for the proxy check contains the logging suffix
// with the full resolved log path.
func TestStatus_JSON_ProxyDetail_LoggingSuffix(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Gateway mode with a log path.
	const logRel = "makeslop-requests.log"
	yamlContent := `exclude:
  scan:
    patterns: []
    skip-dirs: []
  files: []
  dirs: []
network:
  proxy:
    address: ""
  log: "` + logRel + `"
`
	if err := os.WriteFile(filepath.Join(pwd, ".makeslop.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write .makeslop.yaml: %v", err)
	}

	installFakeStatusClient(t, false, false)

	stdout, _, _ := runStatusCmd(t, baseDir, "status", "--json")

	var result statusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, stdout)
	}

	var proxyCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "proxy" {
			proxyCheck = &result.Checks[i]
			break
		}
	}
	if proxyCheck == nil {
		t.Fatalf("--json missing 'proxy' check; checks: %+v", result.Checks)
	}

	// The detail must start with "gateway (direct egress)" and include the
	// logging suffix with the full resolved absolute path.
	if !strings.HasPrefix(proxyCheck.Detail, "gateway (direct egress)") {
		t.Errorf("proxy detail must start with 'gateway (direct egress)'; got %q", proxyCheck.Detail)
	}
	if !strings.Contains(proxyCheck.Detail, "logging →") {
		t.Errorf("proxy detail must contain 'logging →' suffix; got %q", proxyCheck.Detail)
	}
	resolvedPwd := evalSymlinks(t, pwd)
	wantLogPath := filepath.Join(resolvedPwd, logRel)
	if !strings.Contains(proxyCheck.Detail, wantLogPath) {
		t.Errorf("proxy detail must contain resolved log path %q; got %q", wantLogPath, proxyCheck.Detail)
	}
}

// ── Socat image check tests ───────────────────────────────────────────────────

// TestStatus_SocatImagePresent verifies that when alpine/socat is locally
// present, the socat-image check is ok with "alpine/socat present" detail.
func TestStatus_SocatImagePresent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon up, app image present, socat image present.
	installFakeStatusClientFull(t, false, false, false /* socatMissing=false */)

	stdout, _, err := runStatusCmd(t, baseDir, "status", "--json")
	if err != nil {
		t.Fatalf("status should exit 0 when all checks pass; err=%v", err)
	}

	var result statusResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", jsonErr, stdout)
	}

	var socatCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "socat-image" {
			socatCheck = &result.Checks[i]
			break
		}
	}
	if socatCheck == nil {
		t.Fatalf("--json missing 'socat-image' check; checks: %+v", result.Checks)
	}
	if socatCheck.State != checkOK {
		t.Errorf("socat-image state = %q, want %q", socatCheck.State, checkOK)
	}
	if socatCheck.Detail != "alpine/socat present" {
		t.Errorf("socat-image detail = %q, want %q", socatCheck.Detail, "alpine/socat present")
	}
}

// TestStatus_SocatImageAbsent verifies that when alpine/socat is not locally
// present, the socat-image check is non-blocking warn with a pull hint.
func TestStatus_SocatImageAbsent(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Daemon up, app image present, socat image absent.
	installFakeStatusClientFull(t, false, false, true /* socatMissing=true */)

	stdout, stderr, err := runStatusCmd(t, baseDir, "status", "--json")

	// The socat-image check is non-blocking — status must still exit 0 when all
	// other blocking checks pass.
	if err != nil {
		t.Errorf("socat-image absent must be non-blocking (exit 0); err=%v stderr=%q", err, stderr)
	}

	var result statusResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", jsonErr, stdout)
	}

	var socatCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "socat-image" {
			socatCheck = &result.Checks[i]
			break
		}
	}
	if socatCheck == nil {
		t.Fatalf("--json missing 'socat-image' check; checks: %+v", result.Checks)
	}
	// Must be warn (non-blocking), not fail.
	if socatCheck.State != checkWarn {
		t.Errorf("socat-image absent state = %q, want %q (non-blocking)", socatCheck.State, checkWarn)
	}
	if !strings.Contains(socatCheck.Detail, "will pull on first run") {
		t.Errorf("socat-image detail must hint at pull; got %q", socatCheck.Detail)
	}
}

// TestStatus_SocatImageAbsent_PlainOutput verifies that the "alpine/socat absent"
// hint appears in plain (non-JSON) status output.
func TestStatus_SocatImageAbsent_PlainOutput(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	installFakeStatusClientFull(t, false, false, true /* socatMissing=true */)

	_, stderr, err := runStatusCmd(t, baseDir, "status")
	// Still exits 0 (non-blocking).
	if err != nil {
		t.Errorf("socat-image absent must not fail status; err=%v stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "socat-image") {
		t.Errorf("plain output must mention 'socat-image'; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "will pull on first run") {
		t.Errorf("plain output must hint at pull; stderr=%q", stderr)
	}
	// Non-blocking: ready verdict must still appear.
	if !strings.Contains(stderr, "ready") {
		t.Errorf("plain output must show 'ready' verdict; stderr=%q", stderr)
	}
	if strings.Contains(stderr, "not ready") {
		t.Errorf("socat-image absent must not cause 'not ready'; stderr=%q", stderr)
	}
}

// TestStatus_SocatImageCheck_InspectError verifies that when ImageExists returns
// a non-not-found error for the socat image, the socat-image check is non-blocking
// (warn, exits 0) with an error detail string.
func TestStatus_SocatImageCheck_InspectError(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	fc := installFakeStatusClient(t, false, false)
	fc.SocatImageErr = errors.New("daemon io error") // non-not-found error specifically for socat image

	stdout, stderr, err := runStatusCmd(t, baseDir, "status", "--json")
	// socat-image error is non-blocking — status must exit 0.
	if err != nil {
		t.Errorf("socat-image inspect error must be non-blocking (exit 0); err=%v stderr=%q", err, stderr)
	}

	var result statusResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", jsonErr, stdout)
	}

	var socatCheck *statusCheck
	for i := range result.Checks {
		if result.Checks[i].Name == "socat-image" {
			socatCheck = &result.Checks[i]
		}
	}
	if socatCheck == nil {
		t.Fatalf("--json missing 'socat-image' check; checks: %+v", result.Checks)
	}
	if socatCheck.State != checkWarn {
		t.Errorf("socat-image inspect-error state = %q, want %q (non-blocking)", socatCheck.State, checkWarn)
	}
	if !strings.Contains(socatCheck.Detail, "error checking socat image") {
		t.Errorf("socat-image detail must mention error; got %q", socatCheck.Detail)
	}
}

// TestStatus_SocatImageCheck_AppearAfterProxy verifies that the socat-image check
// appears after the proxy check in --json output.
func TestStatus_SocatImageCheck_AppearAfterProxy(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	if _, _, err := runCmd(t, baseDir, "init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	installFakeStatusClient(t, false, false)

	stdout, _, _ := runStatusCmd(t, baseDir, "status", "--json")

	var result statusResult
	if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", jsonErr, stdout)
	}

	proxyIdx := -1
	socatIdx := -1
	for i, c := range result.Checks {
		switch c.Name {
		case "proxy":
			proxyIdx = i
		case "socat-image":
			socatIdx = i
		}
	}
	if proxyIdx < 0 {
		t.Fatal("proxy check not found in --json output")
	}
	if socatIdx < 0 {
		t.Fatal("socat-image check not found in --json output")
	}
	if socatIdx <= proxyIdx {
		t.Errorf("socat-image (idx %d) must appear after proxy (idx %d) in checks array", socatIdx, proxyIdx)
	}
}

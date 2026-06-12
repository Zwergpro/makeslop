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

// Fresh init (no prior settings.json) stamps Version = ConfigVersion,
// so a freshly-init'd dir is never reported stale.
func TestInit_FreshSeed_StampsVersion(t *testing.T) {
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
	if s.Version != config.ConfigVersion {
		t.Errorf("Version = %d, want %d (ConfigVersion)", s.Version, config.ConfigVersion)
	}
}

// An existing stale config gets a non-blocking nudge but Version is NOT
// stamped — stamping would skip the actual migration.
func TestInit_StaleConfig_NudgesWithoutStamping(t *testing.T) {
	setHomeToTestParent(t)
	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)

	// Seed a stale settings.json so we hit the "existing-but-stale" path.
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
	if after.Version != 0 {
		t.Errorf("init must not stamp Version on stale dir; got %d, want %d (stale)",
			after.Version, 0)
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
	if s.Version != config.ConfigVersion {
		t.Errorf("Version = %d after build+init, want %d (ConfigVersion); stderr was %q",
			s.Version, config.ConfigVersion, stderr)
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

// init must fail with a clear error when .makeslop.yaml is a symlink (dangling
// or live). Scaffold detects the symlink on the EEXIST path and returns a hard
// error instead of treating the symlink as an idempotent regular-file hit.
func TestInit_SymlinkedProjectConfig_FailsLoud(t *testing.T) {
	skipNonPOSIX(t, "symlinks are POSIX-only; makeslop is POSIX-only")
	setHomeToTestParent(t)

	baseDir := t.TempDir()
	pwd := t.TempDir()
	t.Chdir(pwd)
	resolvedPwd := evalSymlinks(t, pwd)

	// Place a dangling symlink at .makeslop.yaml — the target does not exist.
	configPath := filepath.Join(resolvedPwd, projectconfig.Filename)
	if err := os.Symlink("/nonexistent/target", configPath); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "init")
	if err == nil {
		t.Fatalf("expected init to fail with symlinked .makeslop.yaml, got nil error; stderr=%q", stderr)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected error to mention 'symlink', got: %v", err)
	}
}

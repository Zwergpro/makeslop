package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
)

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

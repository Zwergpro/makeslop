package config

import (
	"strings"
	"testing"
)

// TestConfigSet_ValidImage verifies setting image to a non-empty value.
func TestConfigSet_ValidImage(t *testing.T) {
	s := defaultSettings()
	if err := ConfigSet(s, "image", "myimage:latest"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Image != "myimage:latest" {
		t.Errorf("Image = %q, want %q", s.Image, "myimage:latest")
	}
}

// TestConfigSet_ValidShell verifies setting shell to a non-empty value.
func TestConfigSet_ValidShell(t *testing.T) {
	s := defaultSettings()
	if err := ConfigSet(s, "shell", "/bin/bash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Shell != "/bin/bash" {
		t.Errorf("Shell = %q, want %q", s.Shell, "/bin/bash")
	}
}

// TestConfigSet_TmpDirSize_AcceptedValues verifies the accepted patterns for tmp_dir_size.
func TestConfigSet_TmpDirSize_AcceptedValues(t *testing.T) {
	accepted := []string{"1000m", "2g", "512k", "1048576", "100M", "1G", "1K", "0"}
	for _, v := range accepted {
		t.Run(v, func(t *testing.T) {
			s := defaultSettings()
			if err := ConfigSet(s, "tmp_dir_size", v); err != nil {
				t.Errorf("ConfigSet(tmp_dir_size=%q) got error %v; want nil", v, err)
			}
			if s.TmpDirSize != v {
				t.Errorf("TmpDirSize = %q, want %q", s.TmpDirSize, v)
			}
		})
	}
}

// TestConfigSet_TmpDirSize_RejectedValues verifies that invalid patterns are rejected.
func TestConfigSet_TmpDirSize_RejectedValues(t *testing.T) {
	rejected := []string{"10mb", "abc", "", "50%", "-1", "1 m", "1.5m", "10mb", " 100m"}
	for _, v := range rejected {
		t.Run(v, func(t *testing.T) {
			s := defaultSettings()
			orig := s.TmpDirSize
			err := ConfigSet(s, "tmp_dir_size", v)
			if err == nil {
				t.Errorf("ConfigSet(tmp_dir_size=%q) expected error, got nil", v)
			}
			// Settings must not be mutated on validation failure.
			if s.TmpDirSize != orig {
				t.Errorf("TmpDirSize mutated on error: got %q, want %q", s.TmpDirSize, orig)
			}
		})
	}
}

// TestConfigSet_EmptyImage_Rejected verifies that blank image is rejected.
func TestConfigSet_EmptyImage_Rejected(t *testing.T) {
	for _, v := range []string{"", "   ", "\t"} {
		t.Run("value:"+v, func(t *testing.T) {
			s := defaultSettings()
			orig := s.Image
			err := ConfigSet(s, "image", v)
			if err == nil {
				t.Errorf("ConfigSet(image=%q) expected error, got nil", v)
			}
			if s.Image != orig {
				t.Errorf("Image mutated on error: got %q, want %q", s.Image, orig)
			}
		})
	}
}

// TestConfigSet_EmptyShell_Rejected verifies that blank shell is rejected.
func TestConfigSet_EmptyShell_Rejected(t *testing.T) {
	for _, v := range []string{"", "  "} {
		t.Run("value:"+v, func(t *testing.T) {
			s := defaultSettings()
			orig := s.Shell
			err := ConfigSet(s, "shell", v)
			if err == nil {
				t.Errorf("ConfigSet(shell=%q) expected error, got nil", v)
			}
			if s.Shell != orig {
				t.Errorf("Shell mutated on error: got %q, want %q", s.Shell, orig)
			}
		})
	}
}

// TestConfigSet_UnknownKey_ErrorMentionsValidKeys verifies that an unknown key
// error lists all valid keys.
func TestConfigSet_UnknownKey_ErrorMentionsValidKeys(t *testing.T) {
	s := defaultSettings()
	origImage := s.Image
	origShell := s.Shell
	origTmpDirSize := s.TmpDirSize

	err := ConfigSet(s, "bogus", "x")
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	msg := err.Error()
	for _, key := range []string{"image", "shell", "tmp_dir_size"} {
		if !strings.Contains(msg, key) {
			t.Errorf("error message %q missing valid key %q", msg, key)
		}
	}
	if !strings.Contains(msg, "bogus") {
		t.Errorf("error message %q should mention unknown key %q", msg, "bogus")
	}

	// Settings must not be mutated on unknown key.
	if s.Image != origImage {
		t.Errorf("Image mutated: %q", s.Image)
	}
	if s.Shell != origShell {
		t.Errorf("Shell mutated: %q", s.Shell)
	}
	if s.TmpDirSize != origTmpDirSize {
		t.Errorf("TmpDirSize mutated: %q", s.TmpDirSize)
	}
}

// TestConfigList_DefaultSettings verifies that ConfigList returns all three keys
// in registry order with the default values.
func TestConfigList_DefaultSettings(t *testing.T) {
	s := defaultSettings()
	entries := ConfigList(s)

	if len(entries) != 3 {
		t.Fatalf("ConfigList len = %d, want 3", len(entries))
	}

	wantOrder := []ConfigEntry{
		{Name: "image", Value: DefaultImage},
		{Name: "shell", Value: DefaultShell},
		{Name: "tmp_dir_size", Value: DefaultTmpDirSize},
	}
	for i, want := range wantOrder {
		got := entries[i]
		if got.Name != want.Name {
			t.Errorf("entries[%d].Name = %q, want %q", i, got.Name, want.Name)
		}
		if got.Value != want.Value {
			t.Errorf("entries[%d].Value = %q, want %q", i, got.Value, want.Value)
		}
	}
}

// TestConfigList_ReflectsSetValues verifies that ConfigList reflects mutated values.
func TestConfigList_ReflectsSetValues(t *testing.T) {
	s := defaultSettings()
	if err := ConfigSet(s, "image", "custom-img"); err != nil {
		t.Fatalf("set image: %v", err)
	}
	if err := ConfigSet(s, "tmp_dir_size", "2g"); err != nil {
		t.Fatalf("set tmp_dir_size: %v", err)
	}

	entries := ConfigList(s)
	byName := map[string]string{}
	for _, e := range entries {
		byName[e.Name] = e.Value
	}

	if byName["image"] != "custom-img" {
		t.Errorf("image = %q, want %q", byName["image"], "custom-img")
	}
	if byName["tmp_dir_size"] != "2g" {
		t.Errorf("tmp_dir_size = %q, want %q", byName["tmp_dir_size"], "2g")
	}
}

// TestConfigSet_NoMutationOnValidationError verifies that a failed set leaves
// Settings completely unchanged.
func TestConfigSet_NoMutationOnValidationError(t *testing.T) {
	s := defaultSettings()
	s.Image = "before"
	s.Shell = "/bin/sh"
	s.TmpDirSize = "200m"

	// Try invalid tmp_dir_size.
	_ = ConfigSet(s, "tmp_dir_size", "notvalid")
	if s.TmpDirSize != "200m" {
		t.Errorf("TmpDirSize mutated on error: got %q, want %q", s.TmpDirSize, "200m")
	}

	// Try empty image.
	_ = ConfigSet(s, "image", "")
	if s.Image != "before" {
		t.Errorf("Image mutated on error: got %q, want %q", s.Image, "before")
	}
}

// defaultSettings returns a Settings populated with the standard defaults,
// mirroring what Load returns for a missing file.
func defaultSettings() *Settings {
	return &Settings{
		Version:    CurrentVersion,
		Image:      DefaultImage,
		Shell:      DefaultShell,
		TmpDirSize: DefaultTmpDirSize,
		Workspaces: map[string]Workspace{},
	}
}

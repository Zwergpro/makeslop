package config

import (
	"fmt"
	"regexp"
	"strings"
)

// tmpDirSizeRe validates docker --tmpfs size values: digits with an optional
// single k/K/m/M/g/G suffix (a bare number is bytes).
var tmpDirSizeRe = regexp.MustCompile(`^[0-9]+[kKmMgG]?$`)

// configKey describes a single settable configuration key.
type configKey struct {
	name string
	get  func(*Settings) string
	set  func(*Settings, string) error
}

// ConfigEntry is a key/value pair returned by ConfigList.
type ConfigEntry struct {
	Name  string
	Value string
}

// configKeys is the ordered registry of settable keys; order drives ConfigList
// output and the valid-key list in unknown-key errors.
var configKeys = []configKey{
	{
		name: "image",
		get:  func(s *Settings) string { return s.Image },
		set:  setImage,
	},
	{
		name: "shell",
		get:  func(s *Settings) string { return s.Shell },
		set:  setShell,
	},
	{
		name: "tmp_dir_size",
		get:  func(s *Settings) string { return s.TmpDirSize },
		set:  setTmpDirSize,
	},
}

func setImage(s *Settings, v string) error {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return fmt.Errorf("image: value must not be empty or whitespace-only")
	}
	s.Image = trimmed
	return nil
}

func setShell(s *Settings, v string) error {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return fmt.Errorf("shell: value must not be empty or whitespace-only")
	}
	s.Shell = trimmed
	return nil
}

func setTmpDirSize(s *Settings, v string) error {
	if !tmpDirSizeRe.MatchString(v) {
		return fmt.Errorf("tmp_dir_size: invalid value %q — expected digits with optional suffix k/K/m/M/g/G (e.g. 100m, 2g, 512k); a bare number is interpreted as bytes by docker", v)
	}
	// docker interprets --tmpfs size=0 as unlimited, silently removing the cap.
	if v == "0" {
		return fmt.Errorf("tmp_dir_size: value %q is not allowed; docker interprets size=0 as unlimited (use a positive value, e.g. 100m)", v)
	}
	s.TmpDirSize = v
	return nil
}

// ConfigGet returns the stored value for key, or ("", false) for unknown keys.
func ConfigGet(s *Settings, key string) (string, bool) {
	for _, ck := range configKeys {
		if ck.name == key {
			return ck.get(s), true
		}
	}
	return "", false
}

// ConfigList returns the current value of every settable key in registry order.
func ConfigList(s *Settings) []ConfigEntry {
	entries := make([]ConfigEntry, len(configKeys))
	for i, ck := range configKeys {
		entries[i] = ConfigEntry{Name: ck.name, Value: ck.get(s)}
	}
	return entries
}

// ConfigSet validates and applies a key=value update to s.
// Returns an error for unknown keys or invalid values; s is not mutated on error.
func ConfigSet(s *Settings, key, val string) error {
	for _, ck := range configKeys {
		if ck.name == key {
			return ck.set(s, val)
		}
	}
	names := make([]string, len(configKeys))
	for i, ck := range configKeys {
		names[i] = ck.name
	}
	return fmt.Errorf("unknown config key %q; valid keys: %s", key, strings.Join(names, ", "))
}

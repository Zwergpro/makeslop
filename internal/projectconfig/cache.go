package projectconfig

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// CacheMounts declares opt-in persistent directories and existing host binds.
type CacheMounts struct {
	Shared []string
	Local  []string
	Static []CacheBind
}

// CacheBind preserves the host and container path spellings until CLI resolution.
type CacheBind struct{ Host, Container string }

func decodeCache(node *yaml.Node) (CacheMounts, error) {
	if node.Kind == 0 || isNull(node) {
		return CacheMounts{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return CacheMounts{}, fmt.Errorf("projectconfig: cache must be a mapping")
	}

	// Preserve zero-value slices for absent or empty declarations.
	var result CacheMounts
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, list := node.Content[i], node.Content[i+1]
		group, err := cacheGroupName(key, list, seen)
		if err != nil {
			return CacheMounts{}, err
		}
		if err := result.decodeGroup(group, list); err != nil {
			return CacheMounts{}, err
		}
	}

	return result, nil
}

func cacheGroupName(key, list *yaml.Node, seen map[string]bool) (string, error) {
	// Inspect raw nodes: following aliases here would bypass the strict schema.
	if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
		return "", fmt.Errorf("projectconfig: cache key must be a string")
	}

	group := key.Value
	if seen[group] {
		return "", fmt.Errorf("projectconfig: duplicate cache.%s", group)
	}
	seen[group] = true

	switch group {
	case "shared", "local", "static":
	default:
		return "", fmt.Errorf("projectconfig: unknown cache key %q", group)
	}
	if list.Kind != yaml.SequenceNode {
		return "", fmt.Errorf("projectconfig: cache.%s must be a list", group)
	}

	return group, nil
}

func (c *CacheMounts) decodeGroup(group string, list *yaml.Node) error {
	names := map[string]bool{}
	for i, node := range list.Content {
		entry := fmt.Sprintf("cache.%s[%d]", group, i)
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fmt.Errorf("projectconfig: %s must be a string", entry)
		}

		if group == "static" {
			bind, err := decodeCacheBind(node.Value, entry)
			if err != nil {
				return err
			}
			c.Static = append(c.Static, bind)
			continue
		}

		name, err := decodeCacheName(node.Value, entry, names)
		if err != nil {
			return err
		}
		if group == "shared" {
			c.Shared = append(c.Shared, name)
		} else {
			c.Local = append(c.Local, name)
		}
	}

	return nil
}

func decodeCacheBind(value, entry string) (CacheBind, error) {
	host, target, hasSeparator := strings.Cut(value, ":")
	invalidPath := host == "" || target == "" || strings.Contains(target, ":")
	invalidHome := unsupportedHome(host) || unsupportedHome(target)
	absoluteTarget := strings.HasPrefix(target, "/") || target == "~" || strings.HasPrefix(target, "~/")

	validPaths := hasSeparator && !invalidPath && !invalidHome
	if !validPaths || !absoluteTarget || hasControl(value) {
		return CacheBind{}, fmt.Errorf("projectconfig: %s must be host:container", entry)
	}

	return CacheBind{Host: host, Container: target}, nil
}

func decodeCacheName(value, entry string, seen map[string]bool) (string, error) {
	invalidRoot := value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~")
	if invalidRoot || strings.Contains(value, ":") || hasControl(value) {
		return "", fmt.Errorf("projectconfig: %s invalid directory name %q", entry, value)
	}

	// Reject traversal before cleaning: a/../b must not silently become b.
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return "", fmt.Errorf("projectconfig: %s traversal in %q", entry, value)
		}
	}

	name := path.Clean(value)
	if seen[name] {
		return "", fmt.Errorf("projectconfig: %s duplicate directory %q", entry, name)
	}
	seen[name] = true

	return name, nil
}

func unsupportedHome(p string) bool {
	return strings.HasPrefix(p, "~") && p != "~" && !strings.HasPrefix(p, "~/")
}

func hasControl(p string) bool {
	return strings.ContainsFunc(p, unicode.IsControl)
}

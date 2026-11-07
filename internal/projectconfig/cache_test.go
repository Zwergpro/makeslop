package projectconfig

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func cacheNode(t *testing.T, text string) *yaml.Node {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{}
	}

	return doc.Content[0]
}

func cacheEntryNode(group, value string) *yaml.Node {
	// Build raw nodes so even NUL/control-character inputs reach our validator.
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: group},
			{
				Kind: yaml.SequenceNode,
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
				},
			},
		},
	}
}

func TestDecodeCache_Valid(t *testing.T) {
	got, err := decodeCache(cacheNode(t, `shared: [.claude, .config//tool/]
local: [docs, .claude]
static: ["./settings.json:~/.settings.json"]`))
	if err != nil {
		t.Fatal(err)
	}

	want := CacheMounts{
		Shared: []CacheDirectory{{Name: ".claude"}, {Name: ".config/tool"}},
		Local:  []CacheDirectory{{Name: "docs"}, {Name: ".claude"}},
		Static: []CacheBind{{Host: "./settings.json", Container: "~/.settings.json"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDecodeCache_InvalidStructure(t *testing.T) {
	cases := []struct{ name, text string }{
		{name: "scalar", text: "yes"},
		{name: "group null", text: "shared: null"},
		{name: "group scalar", text: "shared: x"},
		{name: "number", text: "local: [12]"},
		{name: "boolean", text: "local: [true]"},
		{name: "null entry", text: "local: [null]"},
		{name: "map entry", text: "local: [{}]"},
		{name: "old key", text: "content: true"},
		{name: "unknown", text: "typo: []"},
		{name: "duplicate key", text: "shared: []\nshared: []"},
		{name: "normalized duplicate", text: "local: [docs, docs/]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCache(cacheNode(t, tc.text)); err == nil {
				t.Fatalf("accepted %s", tc.text)
			}
		})
	}
}

func TestDecodeCache_InvalidNames(t *testing.T) {
	names := []string{
		"", "/abs", "~", "~foo", "../x", "a/../b", "./x", "a/./b", "a:b", "a\nb", "a\x00b",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCache(cacheEntryNode("shared", name)); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
}

func TestDecodeCache_InvalidStatic(t *testing.T) {
	entries := []string{
		"missing", ":/x", "x:", "x:/x:invalid", "~foo:/x", "x:~foo", "x:../docs", "x:./../../docs:ro", "x:a/../../docs:rw", "x\n:/x",
	}

	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			if _, err := decodeCache(cacheEntryNode("static", entry)); err == nil {
				t.Fatalf("accepted %q", entry)
			}
		})
	}
}

func TestDecodeCache_RelativeStatic(t *testing.T) {
	for _, target := range []string{"settings.json", "./settings.json", "nested/settings.json", "./nested/../settings.json"} {
		t.Run(target, func(t *testing.T) {
			got, err := decodeCache(cacheEntryNode("static", "~/.makeslop/settings.json:"+target+":ro"))
			if err != nil {
				t.Fatal(err)
			}
			want := []CacheBind{{Host: "~/.makeslop/settings.json", Container: target, ReadOnly: true}}
			if !reflect.DeepEqual(got.Static, want) {
				t.Fatalf("got %#v, want %#v", got.Static, want)
			}
		})
	}
}

func TestDecodeCache_AliasesAndMerges(t *testing.T) {
	cases := []struct{ name, text string }{
		{name: "group alias", text: "shared: &names [a]\nlocal: *names"},
		{name: "entry alias", text: "shared: [&x a, *x]"},
		{name: "merge", text: "<<: {shared: [a]}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCache(cacheNode(t, tc.text)); err == nil {
				t.Fatalf("accepted %s", tc.text)
			}
		})
	}

	alias := &yaml.Node{Kind: yaml.AliasNode, Alias: cacheNode(t, "shared: [a]")}
	if _, err := decodeCache(alias); err == nil {
		t.Fatal("accepted cache alias")
	}
}

func TestDecodeCache_PathCharacters(t *testing.T) {
	got, err := decodeCache(cacheNode(t, `local: ["a b,c'd"]
static: ["./a b,c'd:/opt/a b,c'd"]`))
	if err != nil {
		t.Fatal(err)
	}

	if got.Local[0].Name != "a b,c'd" || got.Static[0].Host != "./a b,c'd" {
		t.Fatalf("mangled: %#v", got)
	}
}

func TestDecodeCache_Modes(t *testing.T) {
	for _, group := range []string{"shared", "local", "static"} {
		for _, suffix := range []string{"", ":ro", ":rw"} {
			t.Run(group+suffix, func(t *testing.T) {
				value := "docs" + suffix
				if group == "static" {
					value = "./docs:/opt/docs" + suffix
				}
				got, err := decodeCache(cacheEntryNode(group, value))
				if err != nil {
					t.Fatal(err)
				}
				readOnly := suffix == ":ro"
				switch group {
				case "shared":
					if !reflect.DeepEqual(got.Shared, []CacheDirectory{{Name: "docs", ReadOnly: readOnly}}) {
						t.Fatalf("got %#v", got.Shared)
					}
				case "local":
					if !reflect.DeepEqual(got.Local, []CacheDirectory{{Name: "docs", ReadOnly: readOnly}}) {
						t.Fatalf("got %#v", got.Local)
					}
				case "static":
					want := []CacheBind{{Host: "./docs", Container: "/opt/docs", ReadOnly: readOnly}}
					if !reflect.DeepEqual(got.Static, want) {
						t.Fatalf("got %#v", got.Static)
					}
				}
			})
		}
	}
}

func TestDecodeCache_InvalidModes(t *testing.T) {
	for _, group := range []string{"shared", "local", "static"} {
		for _, value := range []string{":ro", "docs:", "docs:RO", "docs:other", "docs:ro:rw", "../docs:ro"} {
			if group == "static" {
				value = "./host:/opt/" + value
				// Static destinations permit traversal; exercise an empty path instead.
				if value == "./host:/opt/../docs:ro" || value == "./host:/opt/:ro" {
					value = "./host::ro"
				}
			}
			t.Run(group+"/"+value, func(t *testing.T) {
				if _, err := decodeCache(cacheEntryNode(group, value)); err == nil {
					t.Fatalf("accepted %q", value)
				}
			})
		}
	}
	if _, err := decodeCache(cacheNode(t, "local: [docs, docs/:ro]")); err == nil {
		t.Fatal("accepted duplicate with different mode")
	}
}

func TestDecodeCache_Empty(t *testing.T) {
	for _, text := range []string{"", "null", "{}", "shared: []\nlocal: []\nstatic: []"} {
		t.Run(text, func(t *testing.T) {
			got, err := decodeCache(cacheNode(t, text))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Shared)+len(got.Local)+len(got.Static) != 0 {
				t.Fatalf("nonempty: %#v", got)
			}
		})
	}
}

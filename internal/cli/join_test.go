package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

// joinFixture is a temp tree: <root>/home is $HOME, <home>/app is the main
// project, <home>/.makeslop the data dir, <root>/outside lies outside $HOME.
type joinFixture struct {
	root, home, main, baseDir, outside string
}

func newJoinFixture(t *testing.T) joinFixture {
	t.Helper()
	root := evalSymlinks(t, t.TempDir())
	f := joinFixture{
		root:    root,
		home:    filepath.Join(root, "home"),
		main:    filepath.Join(root, "home", "app"),
		baseDir: filepath.Join(root, "home", ".makeslop"),
		outside: filepath.Join(root, "outside"),
	}
	for _, d := range []string{f.home, f.baseDir, f.outside} {
		mkdirAll(t, d)
	}
	makeProject(t, f.main)
	t.Setenv("HOME", f.home)
	return f
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// makeProject creates dir with an empty .makeslop.yaml and returns dir.
func makeProject(t *testing.T, dir string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, projectconfig.Filename), "")
	return dir
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func (f joinFixture) resolve(raw ...string) ([]joinTarget, error) {
	return resolveJoins(f.main, f.main, "app", f.baseDir, raw, false)
}

func TestParseJoinSuffix(t *testing.T) {
	tests := []struct {
		raw      string
		path     string
		readOnly bool
	}{
		{"../lib", "../lib", false},
		{"../lib:ro", "../lib", true},
		{"../lib:rw", "../lib", false},
		{"foo:bar", "foo:bar", false},
		{"foo:ro:rw", "foo:ro", false},
		{"foo:rw:ro", "foo:rw", true},
		{"foo:RO", "foo:RO", false},
		{"", "", false},
		{":ro", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			path, ro := parseJoinSuffix(tt.raw)
			if path != tt.path || ro != tt.readOnly {
				t.Errorf("parseJoinSuffix(%q) = (%q, %v), want (%q, %v)", tt.raw, path, ro, tt.path, tt.readOnly)
			}
		})
	}
}

func TestResolveJoins_Success(t *testing.T) {
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	tools := makeProject(t, filepath.Join(f.home, "tools"))
	colon := makeProject(t, filepath.Join(f.home, "odd:name"))
	colonRO := makeProject(t, filepath.Join(f.home, "weird:ro"))
	target := makeProject(t, filepath.Join(f.home, "real"))
	symlink(t, target, filepath.Join(f.home, "linked"))

	tests := []struct {
		name string
		raw  string
		want joinTarget
	}{
		{"relative no suffix", "../lib", joinTarget{Host: lib, Name: "lib", ReadOnly: false}},
		{"relative ro", "../lib:ro", joinTarget{Host: lib, Name: "lib", ReadOnly: true}},
		{"relative rw", "../lib:rw", joinTarget{Host: lib, Name: "lib", ReadOnly: false}},
		{"absolute", tools, joinTarget{Host: tools, Name: "tools", ReadOnly: false}},
		{"absolute ro", tools + ":ro", joinTarget{Host: tools, Name: "tools", ReadOnly: true}},
		{"colon without suffix", "../odd:name", joinTarget{Host: colon, Name: "odd:name", ReadOnly: false}},
		{"literal :ro dir escaped", "../weird:ro:rw", joinTarget{Host: colonRO, Name: "weird:ro", ReadOnly: false}},
		{"symlinked dir resolved", "../linked", joinTarget{Host: target, Name: "real", ReadOnly: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.resolve(tt.raw)
			if err != nil {
				t.Fatalf("resolveJoins(%q): %v", tt.raw, err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d joins, want 1", len(got))
			}
			tt.want.Raw = tt.raw
			if got[0] != tt.want {
				t.Errorf("got %+v, want %+v", got[0], tt.want)
			}
		})
	}
}

func TestResolveJoins_MultipleKeepFlagOrder(t *testing.T) {
	f := newJoinFixture(t)
	makeProject(t, filepath.Join(f.home, "b"))
	makeProject(t, filepath.Join(f.home, "a"))
	got, err := f.resolve("../b:ro", "../a")
	if err != nil {
		t.Fatalf("resolveJoins: %v", err)
	}
	if len(got) != 2 || got[0].Name != "b" || !got[0].ReadOnly || got[1].Name != "a" || got[1].ReadOnly {
		t.Errorf("unexpected joins: %+v", got)
	}
}

func TestResolveJoins_NoFlags(t *testing.T) {
	f := newJoinFixture(t)
	got, err := f.resolve()
	if err != nil {
		t.Fatalf("resolveJoins: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no joins, got %+v", got)
	}
}

func TestResolveJoins_PathErrors(t *testing.T) {
	f := newJoinFixture(t)
	mkdirAll(t, filepath.Join(f.home, "plain"))
	if err := os.WriteFile(filepath.Join(f.home, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	linkedCfg := filepath.Join(f.home, "linkedcfg")
	mkdirAll(t, linkedCfg)
	symlink(t, filepath.Join(f.main, projectconfig.Filename), filepath.Join(linkedCfg, projectconfig.Filename))
	dirCfg := filepath.Join(f.home, "dircfg")
	mkdirAll(t, filepath.Join(dirCfg, projectconfig.Filename))

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing dir", "../nope", `--join "../nope": `},
		{"file not dir", "../file", `--join "../file": not a directory`},
		{"no config", "../plain:ro", `--join "../plain:ro": not a makeslop project (no .makeslop.yaml)`},
		{"symlinked config", "../linkedcfg", `--join "../linkedcfg": .makeslop.yaml is a symlink`},
		{"config is a dir", "../dircfg", `--join "../dircfg": .makeslop.yaml is not a regular file`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.resolve(tt.raw)
			if err == nil {
				t.Fatalf("expected error for %q", tt.raw)
			}
			if !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %q, want prefix %q", err, tt.want)
			}
		})
	}
}

// Config errors wrap shared sentinels rather than copying their text.
func TestResolveJoins_ConfigSentinels(t *testing.T) {
	f := newJoinFixture(t)
	mkdirAll(t, filepath.Join(f.home, "plain"))
	linked := filepath.Join(f.home, "linked")
	mkdirAll(t, linked)
	symlink(t, filepath.Join(f.main, projectconfig.Filename), filepath.Join(linked, projectconfig.Filename))

	if _, err := f.resolve("../plain"); !errors.Is(err, errNotProject) {
		t.Errorf("no config: err = %v, want wrapping errNotProject", err)
	}
	if _, err := f.resolve("../linked"); !errors.Is(err, projectconfig.ErrConfigSymlink) {
		t.Errorf("symlinked config: err = %v, want wrapping projectconfig.ErrConfigSymlink", err)
	}
}

func TestResolveJoins_OverlapErrors(t *testing.T) {
	f := newJoinFixture(t)
	makeProject(t, filepath.Join(f.home, "lib"))
	makeProject(t, filepath.Join(f.home, "lib", "sub"))
	makeProject(t, filepath.Join(f.main, "inner"))
	makeProject(t, filepath.Join(f.baseDir, "proj"))

	tests := []struct {
		name string
		raw  []string
		want string
	}{
		{"join is main", []string{"."}, `--join ".": is the current project`},
		{"join is main by abs path", []string{f.main + ":ro"}, `--join "` + f.main + `:ro": is the current project`},
		{"empty value is pwd", []string{""}, `--join "": is the current project`},
		{"empty ro value is pwd", []string{":ro"}, `--join ":ro": is the current project`},
		{"join inside main", []string{"inner"}, `--join "inner": is inside the current project`},
		{"duplicate", []string{"../lib", "../lib:ro"}, `--join "../lib:ro": overlaps --join "../lib"`},
		{"duplicate different spelling", []string{"../lib", "./../lib/"}, `--join "./../lib/": overlaps --join "../lib"`},
		{"nested join inside earlier", []string{"../lib", "../lib/sub"}, `--join "../lib/sub": overlaps --join "../lib"`},
		{"nested join contains earlier", []string{"../lib/sub", "../lib"}, `--join "../lib": overlaps --join "../lib/sub"`},
		{"join inside baseDir", []string{"../.makeslop/proj"}, `--join "../.makeslop/proj": overlaps the makeslop data dir ` + f.baseDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.resolve(tt.raw...)
			if err == nil {
				t.Fatalf("expected error for %q", tt.raw)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveJoins_JoinContainsMain(t *testing.T) {
	f := newJoinFixture(t)
	outer := makeProject(t, filepath.Join(f.home, "outer"))
	inner := makeProject(t, filepath.Join(outer, "app"))
	_, err := resolveJoins(inner, inner, "app", f.baseDir, []string{".."}, false)
	if err == nil || err.Error() != `--join "..": contains the current project` {
		t.Errorf("error = %v", err)
	}
}

func TestResolveJoins_JoinContainsBaseDir(t *testing.T) {
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	baseDir := filepath.Join(lib, ".makeslop")
	mkdirAll(t, baseDir)
	_, err := resolveJoins(f.main, f.main, "app", baseDir, []string{"../lib"}, false)
	want := `--join "../lib": overlaps the makeslop data dir ` + baseDir
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestResolveJoins_NameErrors(t *testing.T) {
	f := newJoinFixture(t)
	makeProject(t, filepath.Join(f.home, "x", "lib"))
	makeProject(t, filepath.Join(f.home, "y", "lib"))
	makeProject(t, filepath.Join(f.home, "z", "app"))
	makeProject(t, filepath.Join(f.home, "w"))
	makeProject(t, filepath.Join(f.home, "w", "lib"))
	makeProject(t, filepath.Join(f.home, "w", "app"))

	tests := []struct {
		name string
		raw  []string
		want string
	}{
		{"basename collision between joins", []string{"../x/lib", "../y/lib:ro"},
			`--join "../y/lib:ro": mount name "lib" collides with --join "../x/lib"`},
		{"basename collision with main", []string{"../z/app"},
			`--join "../z/app": mount name "app" collides with the current project`},
		{"root dir", []string{"/"}, `--join "/": cannot derive a mount name from /`},
		// Overlap with any earlier join wins over a name collision.
		{"overlap with later join beats name clash with earlier", []string{"../x/lib", "../w", "../w/lib"},
			`--join "../w/lib": overlaps --join "../w"`},
		{"overlap with join beats name clash with main", []string{"../w", "../w/app"},
			`--join "../w/app": overlaps --join "../w"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.resolve(tt.raw...)
			if err == nil {
				t.Fatalf("expected error for %q", tt.raw)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveJoins_RootDirRejectedEvenOutOfHome(t *testing.T) {
	f := newJoinFixture(t)
	_, err := resolveJoins(f.main, f.main, "app", f.baseDir, []string{"/:ro"}, true)
	if err == nil || err.Error() != `--join "/:ro": cannot derive a mount name from /` {
		t.Errorf("error = %v", err)
	}
}

func TestResolveJoins_HomeGuard(t *testing.T) {
	f := newJoinFixture(t)
	ext := makeProject(t, filepath.Join(f.outside, "ext"))

	_, err := resolveJoins(f.main, f.main, "app", f.baseDir, []string{ext + ":ro"}, false)
	want := `--join "` + ext + `:ro": outside ` + f.home + ` — pass --out-of-home to override`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}

	got, err := resolveJoins(f.main, f.main, "app", f.baseDir, []string{ext + ":ro"}, true)
	if err != nil {
		t.Fatalf("with outOfHome: %v", err)
	}
	if len(got) != 1 || got[0].Host != ext || !got[0].ReadOnly {
		t.Errorf("unexpected joins: %+v", got)
	}
}

func TestResolveJoins_HomeGuardSymlinkIntoOutside(t *testing.T) {
	f := newJoinFixture(t)
	ext := makeProject(t, filepath.Join(f.outside, "ext"))
	symlink(t, ext, filepath.Join(f.home, "ext"))
	_, err := f.resolve("../ext")
	if err == nil || !strings.Contains(err.Error(), "outside "+f.home) {
		t.Errorf("symlink pointing outside $HOME must trip the guard; got %v", err)
	}
}

// mainRoot reached through an alias path: the lexical check misses it, the
// inode check does not.
func TestResolveJoins_InodeOverlap(t *testing.T) {
	f := newJoinFixture(t)
	alias := filepath.Join(f.home, "alias")
	symlink(t, f.main, alias)
	makeProject(t, filepath.Join(f.main, "inner"))
	outer := makeProject(t, filepath.Join(f.home, "outer"))
	makeProject(t, filepath.Join(outer, "deep"))
	outerAlias := filepath.Join(f.home, "outeralias")
	symlink(t, outer, outerAlias)

	tests := []struct {
		name     string
		mainRoot string
		raw      string
		want     string
	}{
		{"same dir via alias", alias, "../app", `--join "../app": is the current project`},
		{"inside main via alias", alias, "../app/inner", `--join "../app/inner": is inside the current project`},
		{"contains main via alias", filepath.Join(outerAlias, "deep"), "../outer", `--join "../outer": contains the current project`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveJoins(f.main, tt.mainRoot, "main", f.baseDir, []string{tt.raw}, false)
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveJoins_InodeOverlapBaseDir(t *testing.T) {
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	aliasBase := filepath.Join(f.home, "basealias")
	symlink(t, lib, aliasBase)
	_, err := resolveJoins(f.main, f.main, "app", aliasBase, []string{"../lib"}, false)
	want := `--join "../lib": overlaps the makeslop data dir ` + aliasBase
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestHasSameFileAncestor(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	parent := filepath.Join(root, "p")
	child := filepath.Join(parent, "a", "b")
	mkdirAll(t, child)
	other := filepath.Join(root, "other")
	mkdirAll(t, other)
	alias := filepath.Join(root, "alias")
	symlink(t, parent, alias)

	tests := []struct {
		name          string
		parent, child string
		want          bool
	}{
		{"self", parent, parent, true},
		{"descendant", parent, child, true},
		{"alias parent", alias, child, true},
		{"alias child", parent, filepath.Join(alias, "a"), true},
		{"sibling", other, child, false},
		{"child is ancestor", child, parent, false},
		{"missing parent", filepath.Join(root, "missing"), child, false},
		{"missing child under parent", parent, filepath.Join(parent, "missing", "x"), true},
		{"missing child elsewhere", parent, filepath.Join(other, "missing"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSameFileAncestor(tt.parent, tt.child); got != tt.want {
				t.Errorf("hasSameFileAncestor(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/bc", false},
		{"/a/bc", "/a/b", false},
		{"/", "/anything", true},
		{"/x/y", "/z", false},
	}
	for _, tt := range tests {
		if got := overlaps(tt.a, tt.b); got != tt.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIsWithinHome(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	home := filepath.Join(root, "home")
	mkdirAll(t, home)
	linkHome := filepath.Join(root, "linkhome")
	symlink(t, home, linkHome)

	tests := []struct {
		name    string
		envHome string
		path    string
		want    bool
	}{
		{"home itself", home, home, true},
		{"below home", home, filepath.Join(home, "a", "b"), true},
		{"outside", home, filepath.Join(root, "other"), false},
		{"prefix sibling", home, home + "2", false},
		{"parent", home, root, false},
		{"symlinked HOME resolved", linkHome, filepath.Join(home, "proj"), true},
		// Lexically outside, but an ancestor is $HOME by inode (stands in for a
		// differently cased spelling on a case-insensitive filesystem).
		{"alias of home by inode", home, filepath.Join(linkHome, "proj"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", tt.envHome)
			ok, gotHome, err := isWithinHome(tt.path)
			if err != nil {
				t.Fatalf("isWithinHome: %v", err)
			}
			if ok != tt.want {
				t.Errorf("isWithinHome(%q) = %v, want %v", tt.path, ok, tt.want)
			}
			if gotHome != home {
				t.Errorf("home = %q, want resolved %q", gotHome, home)
			}
		})
	}
}

// The inode fallback lives in isWithinHome, so the main run/init guard
// shares it with --join.
func TestEnsureWithinHome_InodeFallback(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	home := filepath.Join(root, "home")
	mkdirAll(t, home)
	alias := filepath.Join(root, "alias")
	symlink(t, home, alias)
	t.Setenv("HOME", home)

	var stderr strings.Builder
	if err := ensureWithinHome(&stderr, filepath.Join(alias, "proj"), false); err != nil {
		t.Errorf("ensureWithinHome(alias/proj) = %v, want nil; stderr=%q", err, stderr.String())
	}
	if err := ensureWithinHome(&stderr, filepath.Join(root, "other"), false); !errors.Is(err, errSilent) {
		t.Errorf("ensureWithinHome(outside) = %v, want errSilent", err)
	}
}

func TestIsWithinHome_UnresolvableHome(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing"))
	if _, _, err := isWithinHome("/"); err == nil {
		t.Error("expected error for nonexistent $HOME")
	}
}

// Relative values resolve against pwd (the cwd), not the main project root.
func TestResolveJoins_RelativeToCwdNotRoot(t *testing.T) {
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	sub := filepath.Join(f.main, "sub")
	mkdirAll(t, sub)

	got, err := resolveJoins(sub, f.main, "app", f.baseDir, []string{"../../lib"}, false)
	if err != nil {
		t.Fatalf("resolveJoins from sub: %v", err)
	}
	if len(got) != 1 || got[0].Host != lib {
		t.Errorf("got %+v, want host %s", got, lib)
	}

	_, err = resolveJoins(sub, f.main, "app", f.baseDir, []string{".."}, false)
	if want := `--join "..": is the current project`; err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestResolveJoins_MissingDirWrapsNotExist(t *testing.T) {
	f := newJoinFixture(t)
	_, err := f.resolve("../nope")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want fs.ErrNotExist wrapped", err)
	}
}

func TestResolveJoins_ConfigLstatError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	// Search permission removed: lib itself stats, its config does not.
	if err := os.Chmod(lib, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lib, 0o755) })

	_, err := f.resolve("../lib")
	if err == nil || !strings.HasPrefix(err.Error(), `--join "../lib": `) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error = %v, want wrapped permission error", err)
	}
}

func TestResolveJoins_HomeResolveErrorWrapped(t *testing.T) {
	f := newJoinFixture(t)
	makeProject(t, filepath.Join(f.home, "lib"))
	t.Setenv("HOME", filepath.Join(f.root, "missing-home"))

	_, err := resolveJoins(f.main, f.main, "app", f.baseDir, []string{f.home + "/lib"}, false)
	want := `--join "` + f.home + `/lib": evaluate symlinks for `
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %v, want prefix %q", err, want)
	}
}

// Join-vs-join overlap compares by inode too: an alias of an earlier join's
// host is caught even when the paths differ lexically.
func TestOverlaps_InodeAlias(t *testing.T) {
	f := newJoinFixture(t)
	lib := makeProject(t, filepath.Join(f.home, "lib"))
	mkdirAll(t, filepath.Join(lib, "sub"))
	alias := filepath.Join(f.home, "libalias")
	symlink(t, lib, alias)

	if !overlaps(alias, filepath.Join(lib, "sub")) {
		t.Error("overlaps(alias, lib/sub) = false, want true")
	}
	if !overlaps(filepath.Join(lib, "sub"), alias) {
		t.Error("overlaps(lib/sub, alias) = false, want true")
	}
	if overlaps(alias, f.main) {
		t.Error("overlaps(alias, main) = true, want false")
	}
}

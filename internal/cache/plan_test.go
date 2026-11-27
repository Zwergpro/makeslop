package cache

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func TestPlan_MountsCopyAndDeclaredExclude(t *testing.T) {
	src, layout := cacheFixture(t)
	cfg := projectconfig.CacheMounts{Local: []projectconfig.CacheDirectory{{Name: "build"}}}
	p, err := Resolve(cfg, src, layout, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Mounts()[0].Container = "/changed"

	if got := p.Mounts()[0].Container; got != "/workspace/app-abc123/build/" {
		t.Fatalf("mounts aliased: %q", got)
	}
	if _, err := Resolve(cfg, src, layout, []string{"build"}); err == nil {
		t.Fatal("accepted missing declared exclude")
	}
}

func TestPlan_PrepareRevalidatesAll(t *testing.T) {
	skipNonPOSIX(t, "symlinks")
	src, layout := cacheFixture(t)
	cfg := projectconfig.CacheMounts{Local: []projectconfig.CacheDirectory{{Name: "first"}, {Name: "second"}}}
	p, err := Resolve(cfg, src, layout, nil)
	if err != nil {
		t.Fatal(err)
	}
	mkdirAll(t, src.WorkspaceDir)
	symlink(t, t.TempDir(), filepath.Join(src.WorkspaceDir, "second"))

	if err := p.Prepare(context.Background()); err == nil {
		t.Fatal("accepted newly inserted symlink")
	}
	assertAbsent(t, filepath.Join(src.WorkspaceDir, "first"))
}

func TestPlan_PrepareEmpty(t *testing.T) {
	if err := (Plan{}).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

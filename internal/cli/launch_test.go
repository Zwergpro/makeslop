package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/docker"
)

func testBuildForeground(t *testing.T, in docker.BuildInputs) docker.ForegroundSpec {
	t.Helper()

	spec, err := docker.BuildForeground(in)
	if err != nil {
		t.Fatal(err)
	}

	return spec
}

// previewFlags extracts single-line flags for these fixtures (not a general shell parser).
func previewFlags(preview, flag string) []string {
	var values []string

	for line := range strings.SplitSeq(preview, "\n") {
		if value, ok := strings.CutPrefix(line, "  "+flag+" "); ok {
			value = strings.TrimSuffix(value, " \\")
			values = append(values, strings.Trim(value, "'"))
		}
	}

	return values
}

func bindFlag(source, target string, readOnly bool) string {
	flag := "type=bind,source=" + source + ",target=" + target

	if readOnly {
		flag += ",readonly"
	}

	return flag
}

func hasBind(preview, source, target string, readOnly bool) bool {
	return slices.Contains(previewFlags(preview, "--mount"), bindFlag(source, target, readOnly))
}

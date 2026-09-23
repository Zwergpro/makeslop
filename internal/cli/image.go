package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Zwergpro/makeslop/internal/config"
)

const noImageHint = "no image configured — run 'makeslop config set image <ref>'"

var errNoImage = errors.New(noImageHint + " or pass -i/--image")

func imageSet(s string) bool {
	return strings.TrimSpace(s) != ""
}

func resolveImage(flagVal, settingsImage string) (string, error) {
	for _, v := range []string{flagVal, settingsImage} {
		ref, err := config.NormalizeImage(v)
		if err != nil {
			return "", err
		}
		if ref != "" {
			return ref, nil
		}
	}
	return "", errNoImage
}

func imageNotFoundHint(ref string) string {
	return fmt.Sprintf("image %[1]q not found locally — build or pull it (e.g. 'docker pull %[1]s')", ref)
}

package cli

import (
	"errors"
	"strings"
)

var errNoImage = errors.New("no image configured — run 'makeslop config set image <ref>' or pass -i/--image")

// resolveImage picks the container image: the -i/--image flag wins, then the
// settings image; both empty (after trimming) yields errNoImage.
func resolveImage(flagVal, settingsImage string) (string, error) {
	if img := strings.TrimSpace(flagVal); img != "" {
		return img, nil
	}
	if img := strings.TrimSpace(settingsImage); img != "" {
		return img, nil
	}
	return "", errNoImage
}

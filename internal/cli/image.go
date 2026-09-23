package cli

import (
	"errors"
	"fmt"
	"strings"
)

// noImageHint is the shared "no image" message; errNoImage, the init note, and
// the status detail are composed from it.
const noImageHint = "no image configured — run 'makeslop config set image <ref>'"

var errNoImage = errors.New(noImageHint + " or pass -i/--image")

// imageSet reports whether s names an image (non-empty after trimming).
func imageSet(s string) bool {
	return strings.TrimSpace(s) != ""
}

// resolveImage picks the container image: the -i/--image flag wins, then the
// settings image; both empty (after trimming) yields errNoImage.
func resolveImage(flagVal, settingsImage string) (string, error) {
	if imageSet(flagVal) {
		return strings.TrimSpace(flagVal), nil
	}
	if imageSet(settingsImage) {
		return strings.TrimSpace(settingsImage), nil
	}
	return "", errNoImage
}

// imageNotFoundHint is the message for a resolved image absent from the local daemon.
func imageNotFoundHint(ref string) string {
	return fmt.Sprintf("image %[1]q not found locally — build or pull it (e.g. 'docker pull %[1]s')", ref)
}

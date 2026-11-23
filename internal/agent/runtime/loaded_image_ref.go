package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ParseLoadedImageRef extracts the image reference a runtime's archive load
// reported on stdout. It understands docker's "Loaded image: <ref>" (and the
// ID-only "Loaded image ID: <id>" form) and nerdctl's "unpacking <ref>". The last
// reference wins, because a multi-image archive ends with the entry the
// runtime finished importing. For an untagged archive, only a single distinct
// image ID identifies the content to tag.
func ParseLoadedImageRef(output string) string {
	return ParseLoadedImageRefFor(output, "")
}

// ParseLoadedImageRefFor prefers the requested image when a multi-image load
// reported it. Otherwise it retains the last reported reference for the caller's
// retag fallback. Equivalent Docker Hub spellings refer to the requested image.
func ParseLoadedImageRefFor(output, image string) string {
	wanted := loadedImageRefName(image)
	found := ""
	var imageID string
	var ambiguousID bool
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		switch {
		case strings.HasPrefix(line, "Loaded image ID:"):
			id := strings.TrimSpace(strings.TrimPrefix(line, "Loaded image ID:"))
			if !IsImageID(id) || (imageID != "" && imageID != id) {
				ambiguousID = true
			}
			imageID = id
			continue
		case strings.HasPrefix(line, "Loaded image:"):
			if ref := strings.TrimSpace(strings.TrimPrefix(line, "Loaded image:")); ref != "" {
				found = ref
			}
		case strings.HasPrefix(line, "unpacking "):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "unpacking "))
			if open := strings.LastIndex(rest, " ("); open > 0 {
				rest = rest[:open]
			}
			if rest = strings.TrimSpace(rest); rest != "" {
				found = rest
			}
		}
		if wanted != "" && loadedImageRefName(found) == wanted {
			return image
		}
	}
	// A requested tag would already have matched above. Mixed tagged and
	// untagged output has no single identity for a manual destination.
	if ambiguousID || (imageID != "" && found != "") {
		return ""
	}
	if imageID != "" {
		return imageID
	}
	return found
}

// IsImageID reports whether ref is a complete SHA-256 image content ID.
func IsImageID(ref string) bool {
	value, ok := strings.CutPrefix(ref, "sha256:")
	if !ok || len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// loadedImageRefName applies Docker name defaults: the legacy index.docker.io
// name, library/ and the latest tag. Registry endpoint hosts remain distinct.
func loadedImageRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "@") && strings.LastIndex(ref, ":") <= strings.LastIndex(ref, "/") {
		ref += ":latest"
	}
	host, path, slash := strings.Cut(ref, "/")
	if !slash || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, path = "docker.io", ref
	}
	switch host {
	case "docker.io", "index.docker.io":
		host = "docker.io"
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
	}
	return host + "/" + path
}

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ParseLoadedImageRef returns the sole identity reported by an archive load.
func ParseLoadedImageRef(output string) string {
	return ParseLoadedImageRefFor(output, "")
}

// ParseLoadedImageRefFor prefers the requested tag. Only an unambiguous
// alternative can safely be tagged as the requested catalog image.
func ParseLoadedImageRefFor(output, image string) string {
	wanted := loadedImageRefName(image)
	var found, identity string
	var ambiguous bool
	var index string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		var ref, key string
		switch {
		case strings.HasPrefix(line, "application/vnd.oci.image.index.v1+json "), strings.HasPrefix(line, "application/vnd.docker.distribution.manifest.list.v2+json "):
			_, digest, _ := strings.Cut(line, " ")
			if IsImageID(digest) {
				if index != "" && index != "import@"+digest {
					ambiguous = true
				}
				index = "import@" + digest
			}
			continue
		case strings.HasPrefix(line, "Loaded image ID:"):
			ref = strings.TrimSpace(strings.TrimPrefix(line, "Loaded image ID:"))
			if !IsImageID(ref) {
				ambiguous = true
				continue
			}
			key = ref
		case strings.HasPrefix(line, "Loaded image:"):
			ref = strings.TrimSpace(strings.TrimPrefix(line, "Loaded image:"))
		case strings.HasPrefix(line, "unpacking "):
			ref, _, _ = strings.Cut(strings.TrimPrefix(line, "unpacking "), " (")
			ref = strings.TrimSpace(ref)
		default:
			continue
		}
		// nerdctl 2.x includes the import index alongside the actual image
		// names. An unnamed child is not a usable source for nerdctl tag.
		if ref == "" || ref == index {
			continue
		}
		if strings.HasPrefix(ref, "import@") {
			ambiguous = true
			continue
		}
		if strings.HasSuffix(ref, ":") {
			// nerdctl 1.x prints an empty tag after unpacking a digest reference.
			if repository, _, ok := strings.Cut(found, "@"); !ok || ref != repository+":" {
				ambiguous = true
			}
			continue
		}
		if key == "" {
			key = loadedImageRefName(ref)
			if key == wanted {
				return image
			}
		}
		if identity != "" && identity != key {
			ambiguous = true
		}
		found, identity = ref, key
	}
	if ambiguous {
		return ""
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

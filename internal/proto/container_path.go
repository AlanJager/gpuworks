package proto

import (
	"path"
	"strings"
)

// ContainerPathsOverlap reports whether two container mount targets collide:
// they are the same path, one lies inside the other, or either is "/". It is
// the one overlap rule every mount source is checked by, wherever it comes
// from (a template's model mount, components, assets and dirs at import; a
// deployment's own mounts, the KV disk tier and the platform's files at
// launch), so two checks never disagree about what overlapping means.
func ContainerPathsOverlap(a, b string) bool {
	a, b = path.Clean(strings.TrimSpace(a)), path.Clean(strings.TrimSpace(b))
	if a == "." || b == "." {
		return false
	}
	if a == "/" || b == "/" || a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

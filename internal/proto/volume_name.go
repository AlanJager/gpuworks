package proto

import "regexp"

// volumeNamePattern is the name docker and nerdctl accept for a named volume.
var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ValidVolumeName reports whether name is a runtime volume name. Both sides
// judge MountSpec.Volume by it: anything else would reach `-v <volume>:<target>`
// as a host path (a leading "/") or as extra mount options (a ":").
func ValidVolumeName(name string) bool {
	return volumeNamePattern.MatchString(name)
}

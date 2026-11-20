package proto

import (
	"regexp"
	"strconv"
	"strings"
)

// A managed directory (MountPurposeManagedDir, ADR 0077) is a writable host
// directory an inference template declares. Every node that runs an instance of
// the deployment keeps one per declared name under a group directory that names
// the deployment; the group survives instances being removed and recreated and
// is removed when the server reports the deployment gone.

// CapManagedDirs: the agent prepares MountPurposeManagedDir mounts, reports the
// groups it holds in Hello.ManagedDirGroups and removes released ones.
const CapManagedDirs = "mount.managed_dirs"

// PathKindManagedDirs is the PathUsage kind measuring the filesystem the node
// keeps managed directories on.
const PathKindManagedDirs = "managed_dirs"

// ManagedDirRef names one managed directory: its deployment's group key
// (ManagedDirGroupKey) and the name the template declared.
type ManagedDirRef struct {
	Group string `json:"group"`
	Name  string `json:"name"`
}

// managedDirName is the name a template may give a directory: a short
// lower-case label, safe as one path component on every node.
var managedDirName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidManagedDirName reports whether name may name a managed directory.
func ValidManagedDirName(name string) bool { return managedDirName.MatchString(name) }

// ManagedDirGroupKey is the directory name a deployment's managed directories
// live under on a node: the deployment id and its creation second. The
// creation time keeps a directory of a deployment from one database from being
// handed to a different deployment that got the same id (a reinstalled server,
// a restored backup). Zero id has no group.
func ManagedDirGroupKey(deploymentID uint, createdUnix int64) string {
	if deploymentID == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(deploymentID), 10) + "-" + strconv.FormatInt(createdUnix, 10)
}

// ParseManagedDirGroupKey reads a key ManagedDirGroupKey wrote. ok is false
// for anything else, which neither side may treat as a deployment's group.
func ParseManagedDirGroupKey(key string) (deploymentID uint, createdUnix int64, ok bool) {
	id, created, found := strings.Cut(key, "-")
	if !found {
		return 0, 0, false
	}
	parsedID, err := strconv.ParseUint(id, 10, 32)
	if err != nil || parsedID == 0 {
		return 0, 0, false
	}
	parsedCreated, err := strconv.ParseInt(created, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if ManagedDirGroupKey(uint(parsedID), parsedCreated) != key {
		return 0, 0, false
	}
	return uint(parsedID), parsedCreated, true
}

package storagerules

import (
	"errors"
	"path"
	"strings"
	"unicode"
)

// ValidatePath refuses operating-system roots, regardless of who configured
// the storage. Mounting or probing them can expose credentials or obscure the
// host's running system. The check is shared by the API and agent.
func ValidatePath(p string) error {
	if !path.IsAbs(p) || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return errors.New("storage paths must be absolute directories without control characters")
	}
	// Lexically cleaning a parent segment before following symbolic links
	// changes what the OS will access (link/../etc can resolve outside the
	// permitted root). Reject dot segments before either policy or I/O uses p.
	for _, component := range strings.Split(p, "/") {
		if component == "." || component == ".." {
			return errors.New("storage paths cannot contain . or .. path components")
		}
	}
	p = path.Clean(p)
	if p == "/" {
		return errors.New("the filesystem root cannot be used as storage")
	}
	for _, root := range []string{"/etc", "/proc", "/sys", "/dev", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/run", "/var/run", "/var/lib/docker", "/var/lib/containerd"} {
		if Within(root, p) {
			return errors.New("operating-system directories cannot be used as storage")
		}
	}
	return nil
}

// Within compares whole path components, including equality.
func Within(root, p string) bool {
	root, p = path.Clean(root), path.Clean(p)
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
}

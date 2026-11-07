package storagerules

import (
	"errors"
	"path"
	"strings"
	"unicode"
)

// defaultDeniedPaths are the operating-system directories a storage cannot
// use unless server.yaml replaces this list. Mounting or probing them can
// expose credentials or obscure the host's running system.
var defaultDeniedPaths = []string{"/etc", "/proc", "/sys", "/dev", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/run", "/var/run", "/var/lib/docker", "/var/lib/containerd"}

// DefaultDeniedPaths returns the built-in denylist.
func DefaultDeniedPaths() []string { return append([]string(nil), defaultDeniedPaths...) }

// CheckPathForm refuses paths no storage can use, whatever the denylist:
// relative paths, control characters, dot segments and the filesystem root.
// The agent applies only this check; the denylist is server policy.
func CheckPathForm(p string) error {
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
	if path.Clean(p) == "/" {
		return errors.New("the filesystem root cannot be used as storage")
	}
	return nil
}

// ValidatePath applies CheckPathForm and refuses p at or below any denied
// directory.
func ValidatePath(p string, denied []string) error {
	if err := CheckPathForm(p); err != nil {
		return err
	}
	for _, root := range denied {
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

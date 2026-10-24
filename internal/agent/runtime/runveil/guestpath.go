package runveil

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxSymlinkHops bounds symlink resolution the way the kernel's ELOOP does.
const maxSymlinkHops = 40

// hostPath translates an absolute guest path to the host path PRoot would
// open, honouring bind mounts and resolving symlinks inside the guest (an
// absolute link target is re-rooted, never followed on the host). It is only
// used for pre-flight checks, so an unresolvable path yields "" rather than an
// error: the caller reports what it was looking for.
func hostPath(root string, binds []Bind, guest string) string {
	if !strings.HasPrefix(guest, "/") {
		return ""
	}
	remaining := strings.Split(strings.TrimPrefix(path.Clean(guest), "/"), "/")
	resolved := "/"
	for hops := 0; len(remaining) > 0; {
		component := remaining[0]
		remaining = remaining[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, component)
		host := mapGuest(root, binds, next)
		info, err := os.Lstat(host)
		if err != nil {
			return ""
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if hops++; hops > maxSymlinkHops {
			return ""
		}
		target, err := os.Readlink(host)
		if err != nil {
			return ""
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		remaining = append(strings.Split(target, "/"), remaining...)
	}
	return mapGuest(root, binds, resolved)
}

// mapGuest maps a symlink-free guest path through the bind table (the most
// specific bind wins, as in PRoot) and otherwise into the guest root.
func mapGuest(root string, binds []Bind, guest string) string {
	best := -1
	for i, bind := range binds {
		target := path.Clean(bind.Guest)
		if guest != target && !strings.HasPrefix(guest, strings.TrimSuffix(target, "/")+"/") {
			continue
		}
		if best < 0 || len(target) > len(path.Clean(binds[best].Guest)) {
			best = i
		}
	}
	if best >= 0 {
		rel := strings.TrimPrefix(guest, path.Clean(binds[best].Guest))
		return filepath.Join(binds[best].Host, filepath.FromSlash(rel))
	}
	return filepath.Join(root, filepath.FromSlash(guest))
}

// ResolveGuestPath resolves an existing guest path through the same bind and
// symlink rules used by command preparation. Empty means it cannot be resolved.
func ResolveGuestPath(root string, binds []Bind, guest string) string {
	return hostPath(root, binds, guest)
}

// findExecutable mirrors udocker's pre-flight FileUtil.find_exec: an absolute
// name is checked as is, "./x" and "../x" are relative to the working
// directory, and a bare name is searched in the guest PATH.
func findExecutable(root string, binds []Bind, name, pathEnv, cwd string) bool {
	candidates := []string{}
	switch {
	case strings.HasPrefix(name, "/"):
		candidates = append(candidates, name)
	case strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../"):
		candidates = append(candidates, path.Join(cwd, name))
	default:
		for _, dir := range strings.Split(pathEnv, ":") {
			switch dir {
			case "":
				continue
			case ".":
				dir = cwd
			}
			candidates = append(candidates, path.Join(dir, name))
		}
	}
	for _, candidate := range candidates {
		host := hostPath(root, binds, candidate)
		if host == "" {
			continue
		}
		if info, err := os.Stat(host); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

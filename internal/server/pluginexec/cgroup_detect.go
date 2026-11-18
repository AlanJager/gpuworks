package pluginexec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// ServerLeaf is the cgroup the server moves itself into under its
// delegated service cgroup. cgroup v2 lets only a cgroup without processes
// of its own hand controllers to children, so the server leaves the service
// cgroup to its plugins' parent: <service>/server holds the server,
// <service>/plugins/<id> each plugin.
const ServerLeaf = "server"

// parseCgroupPlace finds where this process sits in the cgroup v2
// hierarchy: the cgroup2 mount point (root) and the process's cgroup
// relative to it (parent), from /proc/self/mountinfo and /proc/self/cgroup.
// ok is false on a host without cgroup v2 (a v1-only host has no "0::"
// line), for the root cgroup, which the server never reorganises, and for
// a cgroup outside the mounted subtree.
func parseCgroupPlace(mountinfo, selfCgroup []byte) (root, parent string, ok bool) {
	var cg string
	for _, line := range strings.Split(string(selfCgroup), "\n") {
		if rest, found := strings.CutPrefix(line, "0::"); found {
			cg = strings.TrimSpace(rest)
			break
		}
	}
	if cg == "" || strings.HasSuffix(cg, " (deleted)") {
		return "", "", false
	}
	mountRoot := ""
	for _, line := range strings.Split(string(mountinfo), "\n") {
		pre, post, found := strings.Cut(line, " - ")
		fields, after := strings.Fields(pre), strings.Fields(post)
		if !found || len(fields) < 5 || len(after) < 1 || after[0] != "cgroup2" {
			continue
		}
		mountRoot, root = unescapeMount(fields[3]), unescapeMount(fields[4])
		break
	}
	if root == "" {
		return "", "", false
	}
	rel := cg
	if mountRoot != "/" {
		if cg != mountRoot && !strings.HasPrefix(cg, mountRoot+"/") {
			return "", "", false
		}
		rel = strings.TrimPrefix(cg, mountRoot)
	}
	rel = strings.Trim(path.Clean("/"+rel), "/")
	// A server that already moved into its leaf (a second start of the
	// plugin host in the same process) places plugins beside it.
	if path.Base(rel) == ServerLeaf {
		rel = strings.Trim(path.Dir(rel), "/.")
	}
	if rel == "" {
		return "", "", false
	}
	return root, rel, true
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// EnterServerLeaf moves process pid (the server) into
// <root>/<parent>/ServerLeaf, so <root>/<parent> holds no process and can
// enable controllers for the plugins' cgroups. The caller has checked that
// the parent is delegated to the server (DetectCgroup).
func EnterServerLeaf(root, parent string, pid int) error {
	leaf := filepath.Join(root, parent, ServerLeaf)
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cgroup: %w", err)
	}
	if err := writeExisting(filepath.Join(leaf, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("cgroup: moving the server into %s: %w", leaf, err)
	}
	return nil
}

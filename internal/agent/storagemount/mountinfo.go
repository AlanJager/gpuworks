package storagemount

import (
	"bufio"
	"io"
	"path/filepath"
	"strings"
)

// MountEntry is one line of /proc/self/mountinfo, reduced to the fields this package uses.
type MountEntry struct {
	MountPoint   string // mount point, octal escapes decoded
	FSType       string // e.g. "nfs4", "ext4"
	Source       string // e.g. "host:/export"
	Options      string // per-mount options, e.g. "rw,relatime"
	SuperOptions string // per-superblock options, e.g. "rw,vers=4.2,rsize=1048576"
}

// ParseMountinfo parses the mountinfo format (proc(5)): fixed fields, optional fields, a lone "-"
// separator, then fstype, source and super options. Octal escapes such as \040 are decoded. Lines
// that do not follow the format are skipped.
func ParseMountinfo(r io.Reader) ([]MountEntry, error) {
	var out []MountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(f) < sep+3 {
			continue
		}
		e := MountEntry{
			MountPoint: unescapeOctal(f[4]),
			Options:    f[5],
			FSType:     f[sep+1],
			Source:     unescapeOctal(f[sep+2]),
		}
		if len(f) > sep+3 {
			e.SuperOptions = f[sep+3]
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// unescapeOctal decodes the \NNN escapes the kernel uses for space, tab, newline and backslash.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// FindMount returns the entry whose mount point is exactly path. When mounts are stacked on the
// same point the last entry wins, as it is the one that is visible.
func FindMount(entries []MountEntry, path string) (MountEntry, bool) {
	want := filepath.Clean(path)
	for i := len(entries) - 1; i >= 0; i-- {
		if filepath.Clean(entries[i].MountPoint) == want {
			return entries[i], true
		}
	}
	return MountEntry{}, false
}

// normalizeSource drops trailing slashes so "host:/a/" and "host:/a" compare equal.
func normalizeSource(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// superOption returns the value of key=value in a comma separated option string.
func superOption(opts, key string) string {
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, key+"="); ok {
			return v
		}
	}
	return ""
}

// mergeOptions joins option lists, dropping repeated items while keeping order.
func mergeOptions(lists ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, o := range strings.Split(l, ",") {
			if o == "" || seen[o] {
				continue
			}
			seen[o] = true
			out = append(out, o)
		}
	}
	return strings.Join(out, ",")
}

package nerdctl

import (
	"os"
	"strings"
)

// expandDeviceDirs turns a --device that names a directory into one --device
// per entry of it. Docker accepts a directory (the RDMA compute network
// passes /dev/infiniband); nerdctl rejects it with "not a device node", which
// failed every RDMA launch on nerdctl nodes. A container path after the host
// path is kept, per entry; anything that is not a readable directory passes
// through unchanged.
func expandDeviceDirs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value, inline := "", false
		switch {
		case arg == "--device" && i+1 < len(args):
			value = args[i+1]
		case strings.HasPrefix(arg, "--device="):
			value, inline = strings.TrimPrefix(arg, "--device="), true
		default:
			out = append(out, arg)
			continue
		}
		if !inline {
			i++
		}
		host, rest, _ := strings.Cut(value, ":")
		entries, ok := deviceDirEntries(host)
		if !ok {
			out = append(out, "--device", value)
			continue
		}
		target, perm, _ := strings.Cut(rest, ":")
		for _, name := range entries {
			device := host + "/" + name
			if target != "" {
				device += ":" + target + "/" + name
				if perm != "" {
					device += ":" + perm
				}
			}
			out = append(out, "--device", device)
		}
	}
	return out
}

// deviceDirEntries lists the entries of path when it is a directory with any.
func deviceDirEntries(path string) ([]string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, len(names) > 0
}

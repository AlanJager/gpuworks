//go:build linux

package selfinstall

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tcpListen is the "st" value /proc/net/tcp gives a listening socket.
const tcpListen = "0A"

// listenerPID finds the process listening on a TCP port: the socket inodes
// /proc/net/tcp{,6} lists in LISTEN on that port, then the process holding one
// of them open. 0 means none was found, which is also the answer for a holder
// whose /proc/<pid>/fd this process may not read.
func listenerPID(port string) int {
	want, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != tcpListen {
				continue
			}
			local := fields[1]
			hexPort := local[strings.LastIndexByte(local, ':')+1:]
			if got, err := strconv.ParseUint(hexPort, 16, 16); err == nil && got == want && fields[9] != "0" {
				inodes["socket:["+fields[9]+"]"] = true
			}
		}
	}
	if len(inodes) == 0 {
		return 0
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && inodes[link] {
				return pid
			}
		}
	}
	return 0
}

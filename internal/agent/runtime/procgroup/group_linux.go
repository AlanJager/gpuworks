//go:build linux

package procgroup

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

func groupStillWorking(pgid int) bool {
	return groupHasLiveMembers("/proc", pgid) || !procShowsAllPIDs()
}

// groupHasLiveMembers checks a group that signal 0 still finds after SIGKILL.
// Only a visible group with every member confirmed exited can be drained.
func groupHasLiveMembers(procDir string, pgid int) bool {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return true
	}
	found := false
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return true
		}

		// comm is parenthesized and can itself contain spaces and parentheses.
		end := bytes.LastIndexByte(raw, ')')
		if end < 0 {
			return true
		}
		fields := bytes.Fields(raw[end+1:])
		// The tail starts with state (field 3), PPID, PGID, then the rest
		// through num_threads (field 20). A transient PGID of -1 is unknown.
		if len(fields) < 18 || len(fields[0]) != 1 {
			return true
		}
		group, err := strconv.Atoi(string(fields[2]))
		if err != nil || group < 0 {
			return true
		}
		if group != pgid {
			continue
		}

		found = true
		state := fields[0][0]
		// An exited leader can still have other threads holding resources.
		if (state != 'Z' && state != 'X') || !bytes.Equal(fields[17], []byte("1")) {
			return true
		}
	}
	return !found
}

// A hidepid mount can omit members rather than returning permission errors.
// A procfs from a different PID namespace also cannot prove the group empty.
func procShowsAllPIDs() bool {
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return false
	}
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	found := false
	for line := range bytes.SplitSeq(mounts, []byte("\n")) {
		fields := bytes.Fields(line)
		if len(fields) < 4 || !bytes.Equal(fields[1], []byte("/proc")) {
			continue
		}
		if !bytes.Equal(fields[2], []byte("proc")) || bytes.Contains(fields[3], []byte("hidepid=")) {
			return false
		}
		found = true
	}
	return found
}

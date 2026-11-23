//go:build linux

package procgroup

import (
	"bytes"
	"errors"
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
		stat, err := readProcStat(procDir, entry.Name())
		if errors.Is(err, errProcessGone) {
			continue
		}
		if err != nil {
			return true
		}
		if stat.pgid != pgid {
			continue
		}

		found = true
		// An exited leader can still have other threads holding resources.
		if (stat.state != 'Z' && stat.state != 'X') || stat.threads != 1 {
			return true
		}
	}
	return !found
}

// processStillWorking reports whether one process is more than an exited one
// waiting to be reaped. A process this one did not start cannot be reaped by
// it, so its zombie must not keep a teardown pending.
func processStillWorking(pid int) bool {
	stat, err := readProcStat("/proc", strconv.Itoa(pid))
	if err != nil {
		// Gone, or nothing readable about it: either way there is nothing
		// left for this process to wait for.
		return false
	}
	return stat.state != 'Z' && stat.state != 'X'
}

// procStat is what this package reads out of /proc/<pid>/stat.
type procStat struct {
	state   byte
	ppid    int
	pgid    int
	threads int
}

var (
	// errProcessGone says the pid has left the process table.
	errProcessGone = errors.New("process is gone")
	// errProcessUnreadable says the pid is there but nothing about it can be
	// concluded. A caller must never read it as "exited".
	errProcessUnreadable = errors.New("process status is unreadable")
)

func readProcStat(procDir, pid string) (procStat, error) {
	raw, err := os.ReadFile(filepath.Join(procDir, pid, "stat"))
	if os.IsNotExist(err) {
		return procStat{}, errProcessGone
	}
	if err != nil {
		return procStat{}, err
	}

	// comm is parenthesized and can itself contain spaces and parentheses.
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return procStat{}, errProcessUnreadable
	}
	fields := bytes.Fields(raw[end+1:])
	// The tail starts with state (field 3), PPID, PGID, then the rest
	// through num_threads (field 20). A transient PGID of -1 is unknown.
	if len(fields) < 18 || len(fields[0]) != 1 {
		return procStat{}, errProcessUnreadable
	}
	ppid, ppidErr := strconv.Atoi(string(fields[1]))
	group, groupErr := strconv.Atoi(string(fields[2]))
	threads, threadsErr := strconv.Atoi(string(fields[17]))
	if ppidErr != nil || groupErr != nil || threadsErr != nil || group < 0 {
		return procStat{}, errProcessUnreadable
	}
	return procStat{state: fields[0][0], ppid: ppid, pgid: group, threads: threads}, nil
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

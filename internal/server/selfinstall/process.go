package selfinstall

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// readPID reads a pid file. A missing, empty or malformed file means "nothing
// started by us is running", which callers treat as a clean state.
func readPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func writePID(path string, pid int) error {
	return writeFileAtomic(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// readProcCmdline reads /proc/<pid>/cmdline; tests replace it to stand in
// for a process caught between fork and exec.
var readProcCmdline = func(pid int) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
}

// cmdline returns a process's argv. Linux exposes it exactly through /proc; on
// macOS the equivalent is `ps`, whose output can be truncated but keeps argv[0]
// intact, which is the part the identity check needs.
//
// An empty /proc entry is a process whose argv cannot be known yet: one still
// inside exec (the kernel sets the new argv only once the binary is loaded,
// which on a busy disk takes seconds), or a kernel thread. ps would answer
// "[comm]" for it, which looks like another program, so nil is returned.
func cmdline(ctx context.Context, run Runner, pid int) []string {
	if raw, err := readProcCmdline(pid); err == nil {
		var argv []string
		for _, field := range strings.Split(string(raw), "\x00") {
			if field != "" {
				argv = append(argv, field)
			}
		}
		return argv
	}
	out, err := run.Output(ctx, Command{Path: "ps", Args: []string{"-p", strconv.Itoa(pid), "-o", "command="}})
	if err != nil {
		return nil
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "[") {
		return nil
	}
	return fields
}

// processIdentity is what a pid file's pid is running.
type processIdentity int

const (
	processGone    processIdentity = iota // no live process
	processOther                          // a live process running another program
	processUnknown                        // alive, but its argv cannot be read yet
	processOurs                           // running the server binary
)

func identifyProcess(ctx context.Context, run Runner, pid int, binaryPath string) processIdentity {
	if pid <= 0 || !processAlive(pid) {
		return processGone
	}
	argv := cmdline(ctx, run, pid)
	switch {
	case len(argv) == 0:
		return processUnknown
	case argv[0] == binaryPath:
		return processOurs
	default:
		return processOther
	}
}

// isOurServer reports whether pid is running the server binary from binaryPath.
// The pid file lives in a directory an operator or another tool could have
// written, so a stale or recycled pid must never be signalled blindly: a
// process whose argv cannot be read is not ours.
func isOurServer(ctx context.Context, run Runner, pid int, binaryPath string) bool {
	return identifyProcess(ctx, run, pid, binaryPath) == processOurs
}

// mayBeOurServer is isOurServer for questions that signal nothing ("is it
// still running?"): a live process whose argv cannot be read yet counts, since
// the server just started is exactly such a process until its exec completes.
func mayBeOurServer(ctx context.Context, run Runner, pid int, binaryPath string) bool {
	id := identifyProcess(ctx, run, pid, binaryPath)
	return id == processOurs || id == processUnknown
}

// waitForExit polls until pid is gone, escalating to SIGKILL when it outstays
// the stop timeout. The wait is counted in polls rather than wall-clock so an
// injected zero-cost sleep keeps tests instant.
func waitForExit(ctx context.Context, deps Deps, pid int) bool {
	const pollInterval = 200 * time.Millisecond
	polls := int(deps.stopTimeout() / pollInterval)
	if polls < 1 {
		polls = 1
	}
	for attempt := 0; attempt < polls; attempt++ {
		if !processAlive(pid) {
			return true
		}
		deps.sleep(ctx, pollInterval)
	}
	if processAlive(pid) {
		_ = signalProcess(pid, "KILL")
		deps.sleep(ctx, pollInterval)
	}
	return !processAlive(pid)
}

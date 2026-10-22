package docker

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/ptyio"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// pullTTYColumns is wide enough that the CLI prints a layer's byte counters
// rather than dropping them for a narrow terminal.
const pullTTYColumns = 200

// pullOutput connects docker pull's stdout and stderr to a terminal when the
// node can allocate one. The CLI prints per-layer byte counters
// ("Downloading [==>  ] 1.2MB/30MB") only to a terminal, and those counters
// are what lets the puller tell a stalled download from a slow one. Without a
// pty it falls back to a pipe and the counters are simply absent. childEnd is
// the parent's copy of the child's side, closed once the child has started.
func pullOutput(cmd *exec.Cmd) (read *os.File, childEnd *os.File, err error) {
	if master, slave, ptyErr := ptyio.Open(); ptyErr == nil {
		_ = ptyio.SetSize(master, 50, pullTTYColumns)
		cmd.Stdout, cmd.Stderr = slave, slave
		return master, slave, nil
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = write, write
	return read, write, nil
}

// terminalControlRE matches the ANSI control sequences the CLI uses to redraw
// a layer's line in place: cursor movement, erase line, show/hide cursor.
var terminalControlRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// scanPullLines reads pull output written to a pipe or a terminal and calls
// onLine with each non-empty line, control sequences removed. A terminal ends
// a redrawn line with a carriage return rather than a newline, so either one
// ends a line. The end of a terminal's output (EIO once the child has exited)
// is the end of the stream, not an error.
func scanPullLines(r io.Reader, onLine func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(scanTerminalLines)
	for sc.Scan() {
		line := bytes.TrimSpace(terminalControlRE.ReplaceAll(sc.Bytes(), nil))
		if len(line) > 0 {
			onLine(string(line))
		}
	}
	if err := sc.Err(); err != nil && !ptyio.IsClosed(err) {
		return err
	}
	return nil
}

// scanTerminalLines is bufio.ScanLines with a carriage return also ending a
// line.
func scanTerminalLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// pullLayerLineRE is a layer's line in docker pull output: its short id, then
// its status ("Downloading [==>  ] 1.2MB/30MB", "Pull complete", ...).
var pullLayerLineRE = regexp.MustCompile(`^[0-9a-f]{12}: (.+)$`)

// pullLayerStatuses are the statuses docker pull prints for a layer (moby's
// distribution transfer and the CLI's progress output). The progress parser
// reads a layer's bytes and its end from them.
var pullLayerStatuses = []string{
	"Pulling fs layer", "Pulling from", "Waiting", "Layer already being pulled by another client",
	"Downloading", "Retrying in", "Verifying Checksum", "Download complete",
	"Extracting", "Pull complete", "Already exists",
}

// pullStatusCheck watches one pull's output for a layer status it does not
// know. A newer docker that renamed how a layer finishes would otherwise leave
// the layer's last count looking like a download that stopped; on the first
// such line the pull is declared unjudgeable instead
// (runtime.PullLineUnreadable), and the line goes to the agent log.
type pullStatusCheck struct {
	unreadable bool
}

// notice returns the line to report before line, if any.
func (c *pullStatusCheck) notice(line string) string {
	if c.unreadable {
		return ""
	}
	m := pullLayerLineRE.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	for _, known := range pullLayerStatuses {
		if strings.HasPrefix(m[1], known) {
			return ""
		}
	}
	c.unreadable = true
	status := m[1]
	if i := strings.IndexAny(status, "[0123456789"); i > 0 {
		status = status[:i]
	}
	status = strings.TrimSpace(status)
	slog.Warn("docker pull progress not understood; stall check off for this pull", "line", line)
	return runtime.PullLineUnreadable("docker layer status " + strconv.Quote(status))
}

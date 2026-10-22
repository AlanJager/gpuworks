package procgroup

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// Logs streams a workload's redirected log file: the last tail lines (all of
// it when tail <= 0), then, when following, whatever the workload writes next.
//
// tail's own stderr stays out of the stream. A follower outlives the removal
// of the file it follows, and tail announcing that the file "has become
// inaccessible" used to be shipped as if the engine had printed it.
func Logs(ctx context.Context, logPath string, follow bool, tail int) (io.ReadCloser, error) {
	if !follow && tail <= 0 {
		return os.Open(logPath)
	}
	if !follow {
		// Fail like os.Open on a missing file instead of handing back an
		// empty stream.
		if _, err := os.Stat(logPath); err != nil {
			return nil, err
		}
	}
	args := []string{"-n", strconv.Itoa(tail), logPath}
	if follow {
		args = []string{"-n", strconv.Itoa(max(tail, 50)), "-F", logPath}
	}
	cmd := exec.CommandContext(ctx, "tail", args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = pw.CloseWithError(cmd.Wait()) }()
	return &tailStream{pr: pr, cmd: cmd}, nil
}

// tailStream is the output of a tail process; closing it ends the process.
type tailStream struct {
	pr  *io.PipeReader
	cmd *exec.Cmd
}

func (s *tailStream) Read(b []byte) (int, error) { return s.pr.Read(b) }

func (s *tailStream) Close() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.pr.Close()
}

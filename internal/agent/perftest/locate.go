package perftest

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/assets"
)

// ErrToolMissing: neither the host's nor the embedded ib_write_bw can run.
// The wrapped detail says why the embedded one could not.
var ErrToolMissing = errors.New(Tool + " is not installed (install perftest)")

// missingError carries why the embedded tool could not run.
type missingError struct{ detail string }

func (e *missingError) Error() string {
	if e.detail == "" {
		return ErrToolMissing.Error()
	}
	return ErrToolMissing.Error() + ": " + e.detail
}

func (e *missingError) Unwrap() error { return ErrToolMissing }

// recheckAfter bounds how often a failed embedded tool is retried: installing
// the host's RDMA user-space libraries makes it runnable without an agent
// restart.
const recheckAfter = 5 * time.Minute

// locator finds ib_write_bw: the host's on PATH first, so a node runs the
// build its vendor tuned, else the one embedded in the agent. The embedded
// build links the host's libibverbs and librdmacm dynamically; it runs only
// where those RDMA user-space libraries are installed.
type locator struct {
	dir      string
	lookPath func(string) (string, error)
	extract  func(dest string) (string, error)
	// version runs "<bin> --version" and returns its stdout and stderr.
	version func(ctx context.Context, bin string) (stdout, stderr []byte, err error)
	now     func() time.Time

	mu        sync.Mutex
	embedded  string // runnable embedded path, once found
	lastErr   error
	checkedAt time.Time
}

func newLocator(dir string) *locator {
	return &locator{
		dir:      dir,
		lookPath: exec.LookPath,
		extract:  assets.ExtractIBWriteBW,
		version: func(ctx context.Context, bin string) ([]byte, []byte, error) {
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, "--version")
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			return stdout.Bytes(), stderr.Bytes(), err
		},
		now: time.Now,
	}
}

func (l *locator) locate() (string, error) {
	if path, err := l.lookPath(Tool); err == nil {
		return path, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.embedded != "" {
		return l.embedded, nil
	}
	if l.lastErr != nil && l.now().Sub(l.checkedAt) < recheckAfter {
		return "", l.lastErr
	}
	l.checkedAt = l.now()
	path, err := l.runnableEmbedded()
	if err != nil {
		l.lastErr = &missingError{detail: err.Error()}
		return "", l.lastErr
	}
	l.embedded, l.lastErr = path, nil
	return path, nil
}

// runnableEmbedded extracts the embedded tool and checks that it starts: a
// host without the RDMA user-space libraries fails in the dynamic loader
// before the tool prints anything. ib_write_bw --version exits non-zero even
// when it works, so the printed version is what counts.
func (l *locator) runnableEmbedded() (string, error) {
	path, err := l.extract(filepath.Join(l.dir, Tool))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, stderr, err := l.version(ctx, path)
	if strings.Contains(string(stdout), "Version:") {
		return path, nil
	}
	msg := strings.Join(lastMeaningful(stderr, 1), "")
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return "", errors.New("embedded " + Tool + " cannot start: " + msg)
}

//go:build linux && dqlite

package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/logging"
)

const (
	// helperBinaryName is installed beside the server executable.
	helperBinaryName = "gpuworks-dqlite"
	// helperBinaryEnv overrides the location, for tests and unusual layouts.
	helperBinaryEnv = "GPUWORKS_DQLITE_BIN"
	// helperStartAttempts covers a restart right after the previous server
	// died: its child still holds the directory lock and the port until the
	// kernel delivers the parent-death signal.
	helperStartAttempts = 25
	helperStartPause    = 100 * time.Millisecond
	helperReadyWait     = 30 * time.Second
	// helperStopWait is how long an interrupted child may take to close its
	// raft directory before it is killed.
	helperStopWait    = 5 * time.Second
	helperStderrLimit = 2048
)

// helperDied runs when the child exits while a Database still depends on it.
// The pools cannot be repaired in place, so the server exits and its service
// manager restarts both processes. Tests replace it.
var helperDied = func(err error) {
	// The records that lead up to this are still queued for the log file.
	logging.Flush()
	fmt.Fprintf(os.Stderr, "dqlite helper exited unexpectedly: %v\n", err)
	os.Exit(1)
}

// helperIdentity is the single JSON line the child prints once it listens.
type helperIdentity struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"`
	// Protocol is the contract version the child speaks.
	Protocol int `json:"protocol"`
}

type helperProcess struct {
	cmd      *exec.Cmd
	reap     chan struct{}
	reapOnce sync.Once
	exited   chan struct{}
	waitErr  error
	guarded  atomic.Bool
	stopping atomic.Bool
}

// spawnHelper starts the child from a goroutine that stays locked to its OS
// thread until the child has exited. The kernel delivers the parent-death
// signal when the thread that forked the child exits, not when the process
// does, and the Go runtime ends a thread whose locked goroutine returns. A
// child started from an ordinary goroutine could therefore be killed under a
// healthy server. The caller must call release once it has finished reading
// the child's pipes.
func spawnHelper(cmd *exec.Cmd) (*helperProcess, error) {
	p := &helperProcess{cmd: cmd, reap: make(chan struct{}), exited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Never unlocked: the thread ends with this goroutine, after the child.
		err := cmd.Start()
		started <- err
		if err != nil {
			return
		}
		<-p.reap
		p.waitErr = cmd.Wait()
		close(p.exited)
		if p.guarded.Load() && !p.stopping.Load() {
			helperDied(p.waitErr)
		}
	}()
	if err := <-started; err != nil {
		return nil, fmt.Errorf("start dqlite helper: %w", err)
	}
	return p, nil
}

// release lets the owning thread collect the child. Wait closes the child's
// pipes, so it must not run before the caller has read them.
func (p *helperProcess) release() { p.reapOnce.Do(func() { close(p.reap) }) }

// discard kills a child that never became usable and collects it.
func (p *helperProcess) discard() {
	p.stopping.Store(true)
	_ = p.cmd.Process.Kill()
	p.release()
	<-p.exited
}

func helperBinary() (string, error) {
	if path := os.Getenv(helperBinaryEnv); path != "" {
		return path, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), helperBinaryName)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not installed beside the server executable (%s)", helperBinaryName, path)
	}
	return path, nil
}

func helperCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin, err := helperBinary()
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(bin, args...)
	} else {
		cmd = exec.CommandContext(ctx, bin, args...)
	}
	// The child must not outlive a server that was killed outright.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd, nil
}

func startHelperUntilReady(ctx context.Context, args []string) (*helperProcess, helperIdentity, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, helperIdentity{}, err
		}
		process, info, err := startHelper(ctx, args)
		if err == nil {
			return process, info, nil
		}
		if !helperStartRetryable(err) || attempt == helperStartAttempts {
			return nil, helperIdentity{}, err
		}
		timer := time.NewTimer(helperStartPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, helperIdentity{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func helperStartRetryable(err error) bool {
	text := err.Error()
	return strings.Contains(text, "lock is held by another process") || strings.Contains(text, "address already in use")
}

// checkHelperProtocol refuses a database process that speaks another version
// of the contract. A process that states none predates the check.
func checkHelperProtocol(reported int) error {
	switch {
	case reported == HelperProtocolVersion:
		return nil
	case reported == 0:
		return fmt.Errorf("dqlite helper did not report a protocol version; this server supports version %d and the helper predates it, install the helper shipped with this server", HelperProtocolVersion)
	default:
		return fmt.Errorf("dqlite helper speaks protocol version %d; this server supports version %d, install the helper shipped with this server", reported, HelperProtocolVersion)
	}
}

// startHelper starts the child and waits for its identity line. The child is
// not bound to ctx after that: only stop ends it.
func startHelper(ctx context.Context, args []string) (*helperProcess, helperIdentity, error) {
	var info helperIdentity
	cmd, err := helperCommand(nil, args...)
	if err != nil {
		return nil, info, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, info, err
	}
	notes := &cappedStderr{}
	cmd.Stderr = notes
	process, err := spawnHelper(cmd)
	if err != nil {
		return nil, info, err
	}
	line := make(chan error, 1)
	go func() {
		raw, err := bufio.NewReader(stdout).ReadBytes('\n')
		if err == nil {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&info)
		}
		line <- err
	}()
	timer := time.NewTimer(helperReadyWait)
	defer timer.Stop()
	var readErr error
	select {
	case <-ctx.Done():
		readErr = ctx.Err()
	case <-timer.C:
		readErr = errors.New("dqlite helper did not report its address in time")
	case err := <-line:
		if err != nil {
			readErr = fmt.Errorf("dqlite helper did not report its address: %w", err)
		} else if err := checkHelperProtocol(info.Protocol); err != nil {
			readErr = err
		} else if info.ID == 0 || info.Address == "" {
			readErr = errors.New("dqlite helper reported an incomplete identity")
		}
	}
	if readErr != nil {
		// Collecting the child closes the pipe, which releases the reader,
		// and completes stderr.
		process.discard()
		if text := strings.TrimSpace(notes.String()); text != "" {
			readErr = fmt.Errorf("%w: %s", readErr, text)
		}
		return nil, helperIdentity{}, readErr
	}
	process.release()
	return process, info, nil
}

// guard makes an exit of the child fatal. Before it, a failed open belongs to
// the caller, which may retry.
func (p *helperProcess) guard() {
	p.guarded.Store(true)
	select {
	case <-p.exited:
		if !p.stopping.Load() {
			helperDied(p.waitErr)
		}
	default:
	}
}

// stop returns only after the child has exited, so the caller may read or
// move the raft directory. A child that ignores the interrupt for wait is
// killed and reported.
func (p *helperProcess) stop(wait time.Duration) error {
	p.stopping.Store(true)
	p.release()
	if wait > 0 {
		_ = p.cmd.Process.Signal(os.Interrupt)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-p.exited:
			return nil
		case <-timer.C:
		}
	}
	_ = p.cmd.Process.Kill()
	<-p.exited
	if wait > 0 {
		return errors.New("dqlite helper ignored the interrupt and was killed")
	}
	return nil
}

// cappedStderr keeps the start of the child's diagnostics for a startup error
// and passes everything through to the server's own stderr.
type cappedStderr struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *cappedStderr) Write(p []byte) (int, error) {
	c.mu.Lock()
	if remain := helperStderrLimit - c.b.Len(); remain > 0 {
		c.b.Write(p[:min(remain, len(p))])
	}
	c.mu.Unlock()
	_, _ = os.Stderr.Write(p)
	return len(p), nil
}

func (c *cappedStderr) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// runHelper runs one of the child's offline commands to completion.
func runHelper(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := helperCommand(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	process, err := spawnHelper(cmd)
	if err != nil {
		return nil, err
	}
	process.release()
	<-process.exited
	if process.waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return nil, errors.New(text)
		}
		return nil, fmt.Errorf("dqlite helper %s: %w", args[0], process.waitErr)
	}
	return stdout.Bytes(), nil
}

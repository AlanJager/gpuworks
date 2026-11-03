package expose

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// HostSpecEnv carries the connector launch request to the helper process. It is
// an environment variable rather than an argument so nothing that could hold a
// credential ends up in a command line another process can read.
const HostSpecEnv = "GPUWORKS_EXPOSE_CONNECTOR_SPEC"

// Spec is what the helper needs to launch a provider's connector.
type Spec struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	// Env is the complete environment for the connector. The caller applies
	// the isolation policy (HOME, TUNNEL_*, proxy variables); the helper passes
	// it through unchanged.
	Env []string `json:"env"`
}

// HelperCommand names the process that supervises the connector: in production
// the gpuworks binary's hidden subcommand, in tests the test binary.
type HelperCommand struct {
	Path string
	Args []string
}

// SelfHelper is the production helper: this binary, whose hidden subcommand
// owns the connector. It is a variable so an in-process harness can supply its
// own entry point — a test binary is not the gpuworks binary and cannot run
// `expose connector-host`.
var SelfHelper = func() HelperCommand {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return HelperCommand{Path: exe, Args: []string{"expose", "connector-host"}}
}

// Child is a supervised connector process.
//
// The connector is not a direct child of the connector service (`gpuworks
// expose run`, which hosts it instead of the server; see ADR 0045): it is a
// grandchild, held by a helper whose stdin is a pipe owned by the service, and
// it shares that helper's process group. That indirection is the whole point —
// a connector that outlives its service keeps serving the public address with
// nobody supervising it, and a service killed with SIGKILL gets no chance to
// clean up. When the service dies for any reason the pipe closes, the helper sees EOF and takes
// the connector down with it. The same mechanism works on Linux and macOS,
// where no parent-death signal exists.
//
// PID is the helper's pid, which is also the pid of the process group both
// processes live in: signalling that group reaches the connector whether or not
// the helper is still there to pass the signal on.
//
// The one window this leaves: a helper that dies and a service that is killed
// before the next pass can notice leaves a connector with nobody watching the
// pipe. That window is one supervisor interval wide, and what closes it is a
// kernel-level parent-death signal, not anything this process can do later.
type Child struct {
	helper *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	err    error
	pid    int
}

// StartChild launches the helper and with it the connector. logs receives the
// connector's own output, which is how the service both rotates it and reads
// the registration lines out of it.
//
// The process is deliberately not bound to a context: a start budget that
// expires while the connector is up and registering would kill a working
// tunnel and mint a new address for no reason. Its lifetime is owned by Stop,
// and by the liveness pipe when this process dies.
func StartChild(helper HelperCommand, spec Spec, logs io.Writer) (*Child, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(helper.Path, helper.Args...)
	cmd.Env = append(os.Environ(), HostSpecEnv+"="+string(raw))
	cmd.Stdout = logs
	cmd.Stderr = logs
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// The helper's own process group, which the connector joins: a signal aimed
	// at the service's group must not reach the connector, and one signal to the
	// helper's group takes down everything the connector spawned in one go.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	c := &Child{helper: cmd, stdin: stdin, done: make(chan struct{}), pid: cmd.Process.Pid}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// PID is the helper's pid, and with it the id of the process group the helper
// and its connector share — the group a stop signals. Liveness is not decided
// from it: the lease is the authority on whether a helper is still there.
func (c *Child) PID() int { return c.pid }

// Wait blocks until the helper — and therefore the connector — is gone, and
// returns the connector's exit status as the helper reported it.
func (c *Child) Wait() error {
	<-c.done
	return c.err
}

// Exited reports whether the connector is already gone.
func (c *Child) Exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// probeTimeout bounds the liveness probe, so a helper that is alive but not
// reading its pipe cannot stall the caller.
const probeTimeout = 2 * time.Second

// HelperAlive reports whether a helper is still there to watch the connector.
//
// Wait cannot answer this question. It waits for the output pipe to close as
// well as for the helper, and the connector holds the write end of that pipe,
// so a connector that outlived its helper keeps Wait — and with it Exited —
// blocked for as long as it runs.
//
// The lease can answer it: writing to the pipe asks the kernel whether a reader
// still exists. A helper that died holds no file descriptors, so the write
// fails with EPIPE. A helper that is merely not draining the pipe looks
// different — the buffer fills and the write times out — and that counts as
// alive, because the process is still there.
func (c *Child) HelperAlive() bool {
	file, ok := c.stdin.(*os.File)
	if !ok {
		return true
	}
	if err := file.SetWriteDeadline(time.Now().Add(probeTimeout)); err != nil {
		// Cannot ask safely: assume the helper is there, which is the answer
		// that keeps the full grace in Stop.
		return true
	}
	defer func() { _ = file.SetWriteDeadline(time.Time{}) }()
	_, err := file.Write([]byte{0})
	switch {
	case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
		return true
	default:
		return false
	}
}

// orphanedConnectorGrace is the grace Stop gives when there is no helper left:
// only the connector's own exit after a signal it has already received, not a
// helper's shutdown path.
const orphanedConnectorGrace = 5 * time.Second

// Stop takes the connector down: closing the pipe tells the helper to end it, a
// TERM moves a helper that missed the pipe, and a KILL of the process group
// covers a connector that ignores both. Wait always reaps.
//
// The connector lives in the helper's process group, so both signals reach it
// whether or not the helper is still there to pass them on. When the helper is
// already gone the grace is cut short: there is no process left for a shutdown
// request to travel through, and waiting would only make the service's shutdown
// pay for a process that no longer exists.
//
// A connector that exits because we asked it to reports the signal that ended
// it (128+SIGTERM, conventionally 143); that is the expected outcome here, so
// it is not an error. Callers that need the status — the supervisor, to tell a
// crash from a stop — take it from Wait before calling Stop.
func (c *Child) Stop(grace time.Duration) error {
	helperAlive := c.HelperAlive()
	// Closing the pipe is the contract; the signals are belt and braces.
	_ = c.stdin.Close()
	_ = signalGroup(c.pid, syscall.SIGTERM)
	if grace <= 0 {
		grace = 20 * time.Second
	}
	if !helperAlive {
		grace = orphanedConnectorGrace
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(grace):
	}
	_ = signalGroup(c.pid, syscall.SIGKILL)
	select {
	case <-c.done:
		if !helperAlive {
			return fmt.Errorf("the connector outlived its helper and had to be killed (%v)", c.err)
		}
		return fmt.Errorf("the connector ignored the shutdown request and had to be killed (%v)", c.err)
	case <-time.After(5 * time.Second):
		return errors.New("the connector process group did not exit after SIGKILL")
	}
}

// signalGroup signals the whole process group led by pid: the helper and the
// connector, plus anything the connector spawned, in one call. A group outlives
// its leader, so the signal lands even when that leader is already dead.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		// A group that is already gone is not an error here; the caller's Wait
		// is what decides whether the cleanup actually happened.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}

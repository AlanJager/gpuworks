package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"gpuworks.ai/gpuworks/internal/server/pluginexec"
)

// Spawner starts one plugin process.
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (Process, error)
}

// SpawnRequest is everything a spawn needs.
type SpawnRequest struct {
	Spec    *Spec
	Profile *pluginexec.Profile
	Env     []string
}

// Process is a started plugin: the host's ends of its two connections, its
// stderr, and its exit.
type Process interface {
	PID() int
	// Serve is the connection on which the plugin serves the host; Host is
	// the one on which it calls the host API.
	Serve() net.Conn
	Host() net.Conn
	Stderr() io.Reader
	// Done is closed once the process exited; Exit is valid after it.
	Done() <-chan struct{}
	Exit() ExitStatus
	// Signal signals the process group.
	Signal(sig os.Signal) error
}

// ExitStatus is how a process ended: Code when it exited, Signal when a
// signal ended it (Code is then -1).
type ExitStatus struct {
	Code   int
	Signal string
}

// trampolineSpawner starts plugins through the server's own binary.
type trampolineSpawner struct{ path string }

func (t trampolineSpawner) Spawn(ctx context.Context, req SpawnRequest) (Process, error) {
	spec := req.Spec
	args := spec.Args
	if len(args) == 0 {
		args = []string{"serve"}
	}
	argv, err := pluginexec.Args(req.Profile, append([]string{spec.Exe}, args...)...)
	if err != nil {
		return nil, err
	}
	serveCore, servePlugin, err := socketPair()
	if err != nil {
		return nil, err
	}
	hostCore, hostPlugin, err := socketPair()
	if err != nil {
		closeAll(serveCore, servePlugin)
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(serveCore, servePlugin, hostCore, hostPlugin)
		return nil, err
	}
	cmd := exec.Command(t.path, argv...)
	cmd.Env = req.Env
	cmd.Dir = spec.DataDir
	cmd.Stderr = stderrW // stdin and stdout stay nil: /dev/null
	cmd.ExtraFiles = []*os.File{servePlugin, hostPlugin}
	cmd.SysProcAttr = sysProcAttr()

	p := &osProcess{done: make(chan struct{}), stderr: stderrR}
	started := make(chan error, 1)
	go func() {
		// Linux sends the parent-death signal when the thread that forked
		// the child exits, and the runtime retires a thread whose locked
		// goroutine ends. This goroutine therefore locks its thread, starts
		// the child and waits for it, and only then exits (taking the
		// thread with it), so no other goroutine can retire the thread
		// while the plugin runs.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		p.pid = cmd.Process.Pid
		p.proc = cmd.Process
		started <- nil
		err := cmd.Wait()
		p.exit = exitStatusOf(cmd.ProcessState, err)
		close(p.done)
	}()
	err = <-started
	closeAll(servePlugin, hostPlugin, stderrW)
	if err != nil {
		closeAll(serveCore, hostCore, stderrR)
		return nil, fmt.Errorf("starting the trampoline: %w", err)
	}
	if p.serve, err = fileConn(serveCore); err == nil {
		p.host, err = fileConn(hostCore)
	}
	if err != nil {
		_ = p.Signal(syscall.SIGKILL)
		<-p.done
		closeAll(stderrR)
		if p.serve != nil {
			_ = p.serve.Close()
		}
		return nil, err
	}
	return p, nil
}

type osProcess struct {
	pid         int
	proc        *os.Process
	serve, host net.Conn
	stderr      *os.File
	done        chan struct{}
	exit        ExitStatus
}

func (p *osProcess) PID() int              { return p.pid }
func (p *osProcess) Serve() net.Conn       { return p.serve }
func (p *osProcess) Host() net.Conn        { return p.host }
func (p *osProcess) Stderr() io.Reader     { return p.stderr }
func (p *osProcess) Done() <-chan struct{} { return p.done }
func (p *osProcess) Exit() ExitStatus      { return p.exit }

func (p *osProcess) Signal(sig os.Signal) error {
	select {
	case <-p.done:
		return os.ErrProcessDone
	default:
	}
	if err := signalGroup(p.pid, sig); err == nil {
		return nil
	}
	return p.proc.Signal(sig)
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		if f != nil {
			_ = f.Close()
		}
	}
}

func fileConn(f *os.File) (net.Conn, error) {
	defer f.Close()
	return net.FileConn(f)
}

func exitStatusOf(st *os.ProcessState, err error) ExitStatus {
	if st == nil {
		return ExitStatus{Code: -1, Signal: errorString(err)}
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Code: -1, Signal: ws.Signal().String()}
	}
	return ExitStatus{Code: st.ExitCode()}
}

func errorString(err error) string {
	if err == nil {
		return "unknown"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.String()
	}
	return err.Error()
}

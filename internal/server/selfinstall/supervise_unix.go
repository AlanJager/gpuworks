//go:build linux || darwin

package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervise starts the server and starts it again, opts.Delay after every
// exit, until ctx ends; it then sends the server SIGTERM, waits for it to exit
// and returns. As pid 1 (the main process of a container) it also reaps the
// orphans the kernel hands it, which an init would.
func Supervise(ctx context.Context, opts SuperviseOptions) error {
	if opts.PIDFile != "" {
		if err := writePID(opts.PIDFile, os.Getpid()); err != nil {
			return fmt.Errorf("record the supervisor pid: %w", err)
		}
		defer removeOwnPID(opts.PIDFile)
	}
	s := &supervisor{opts: opts, reapAll: os.Getpid() == 1}
	stop := context.AfterFunc(ctx, s.terminate)
	defer stop()
	// The parent-death signal a child is started with fires when the thread
	// that started it ends, so every start comes from this one locked thread.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		s.loop(ctx)
	}()
	<-done
	return nil
}

type supervisor struct {
	opts    SuperviseOptions
	reapAll bool

	mu  sync.Mutex
	pid int
}

func (s *supervisor) loop(ctx context.Context) {
	for {
		pid, err := s.start(ctx)
		switch {
		case err != nil:
			s.opts.logf("supervise: start %s: %v", s.opts.Binary, err)
		case pid > 0:
			s.opts.logf("supervise: server pid %d %s", pid, describeWait(s.wait(pid)))
		}
		if ctx.Err() != nil {
			return
		}
		s.opts.logf("supervise: starting the server again in %s", s.opts.delay())
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.opts.delay()):
		}
	}
}

// start launches the server; it returns pid 0 when ctx has already ended.
func (s *supervisor) start(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return 0, nil
	}
	proc, err := os.StartProcess(s.opts.Binary, append([]string{s.opts.Binary}, s.opts.Args...), &os.ProcAttr{
		Env:   append(os.Environ(), SupervisedEnv+"=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   superviseChildAttr(),
	})
	if err != nil {
		return 0, err
	}
	// The child is waited for by pid below (as pid 1 together with every
	// orphan), never through the handle.
	s.pid = proc.Pid
	_ = proc.Release()
	return s.pid, nil
}

// wait blocks until the server exits.
func (s *supervisor) wait(pid int) syscall.WaitStatus {
	target := pid
	if s.reapAll {
		target = -1
	}
	for {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(target, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || got == pid {
			s.mu.Lock()
			s.pid = 0
			s.mu.Unlock()
			return status
		}
	}
}

// terminate asks the running server to stop.
func (s *supervisor) terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pid > 0 {
		_ = syscall.Kill(s.pid, syscall.SIGTERM)
	}
}

func describeWait(status syscall.WaitStatus) string {
	if status.Signaled() {
		return "was killed by " + status.Signal().String()
	}
	return "exited with status " + strconv.Itoa(status.ExitStatus())
}

// removeOwnPID removes the pid file unless another process has taken it over.
func removeOwnPID(path string) {
	raw, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(raw)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(path)
	}
}

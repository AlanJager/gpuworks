//go:build linux

package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type terminalSessionDriver interface {
	ExecPTYSession(context.Context, string, []string) ([]string, func(context.Context) error, error)
}

func TestContainerTerminalCloseOwnsOnlyItsRemoteSession(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	// Execute the real remote scripts as an unprivileged local user. This CLI
	// accepts only the pinned endpoint/exec grammar, with a fresh session per
	// interactive command just as the container runtime creates one remotely.
	installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; if [ "$1" = -i ]; then shift 2; fi; test "$1" = owned-container; shift; exec "$@"`)
	driver, ok := any(New()).(terminalSessionDriver)
	if !ok {
		t.Fatal("Docker PTY must own remote session teardown, not only a local CLI argv")
	}
	type running struct {
		process *exec.Cmd
		done    chan error
		close   func(context.Context) error
		pid     int
	}
	start := func(name string) running {
		t.Helper()
		pidFile := filepath.Join(t.TempDir(), name+".pid")
		argv, closeSession, err := driver.ExecPTYSession(context.Background(), "owned-container", []string{"/bin/sh", "-c", fmt.Sprintf("printf '%%s' \"$$\" > %q; trap '' HUP; read value", pidFile)})
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(argv[0], argv[1:]...)
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		input, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		t.Cleanup(func() {
			_ = input.Close()
			_ = command.Process.Kill()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = closeSession(ctx)
		})
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			raw, err := os.ReadFile(pidFile)
			if err == nil && len(raw) > 0 {
				pid, err := strconv.Atoi(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				return running{command, done, closeSession, pid}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("real shell never wrote its independent PID")
		return running{}
	}
	first, second := start("first"), start("second")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := first.close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	case <-ctx.Done():
		t.Fatal("close did not terminate owned shell")
	}
	if err := syscall.Kill(first.pid, 0); err == nil {
		t.Fatal("owned shell PID still exists after close")
	}
	if err := syscall.Kill(second.pid, 0); err != nil {
		t.Fatalf("another terminal was affected: %v", err)
	}
	if err := second.close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.done:
	case <-ctx.Done():
		t.Fatal("second shell did not terminate")
	}
	if err := first.close(ctx); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestContainerTerminalPreparationRejectsRuntimeFailure(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	installFakeDocker(t, `printf 'permission denied preparing terminal\n' >&2; exit 42`)
	driver, ok := any(New()).(terminalSessionDriver)
	if !ok {
		t.Fatal("Docker PTY lifecycle missing")
	}
	argv, closeSession, err := driver.ExecPTYSession(context.Background(), "owned-container", nil)
	if err == nil || argv != nil || closeSession != nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("prepare failure was ignored: argv=%v err=%v", argv, err)
	}
}

func TestContainerTerminalCloseBeforeStartPreventsLateExec(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; if [ "$1" = -i ]; then shift 2; fi; test "$1" = owned-container; shift; exec "$@"`)
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", []string{"/bin/sh", "-c", fmt.Sprintf("printf wrong > %q", marker)})
	if err != nil {
		t.Fatal(err)
	}
	if err = closeSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("late closed session executed: %s", output)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("late exec created a resource: %v", err)
	}
}

func TestContainerTerminalRejectsForgedIdentityAndRetainsRetryableCleanup(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; if [ "$1" = -i ]; then shift 2; fi; test "$1" = owned-container; shift; exec "$@"`)
	argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := argv[11]
	t.Cleanup(func() { _ = os.Remove(filepath.Join(directory, "pid")); _ = closeSession(context.Background()) })
	for _, bad := range []string{"1 1 1 1", "123 x 123 123", "123 1 456 123", ""} {
		if err = os.WriteFile(filepath.Join(directory, "pid"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = closeSession(context.Background()); err == nil {
			t.Fatalf("unsafe identity accepted: %q", bad)
		}
		if _, err = os.Stat(directory); err != nil {
			t.Fatalf("failed cleanup lost ownership: %v", err)
		}
	}
	if err = os.Remove(filepath.Join(directory, "pid")); err != nil {
		t.Fatal(err)
	}
	if err = closeSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("closed guard remains: %v", err)
	}
}

func TestContainerTerminalPartialPreparationIsReclaimed(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	record := filepath.Join(t.TempDir(), "owned-directory")
	t.Setenv("TERMINAL_PREPARE_RECORD", record)
	installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; test "$1" = owned-container; shift; case "$3" in *'umask 077; mkdir'*) printf '%s' "$5" > "$TERMINAL_PREPARE_RECORD"; mkdir "$5"; echo 'permission denied creating armed marker' >&2; exit 42;; esac; exec "$@"`)
	argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", nil)
	if err == nil || argv != nil || closeSession != nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("partial prepare was hidden: %v", err)
	}
	raw, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, err = os.Stat(string(raw)); !os.IsNotExist(err) {
		t.Fatalf("partial preparation leaked %q: %v", raw, err)
	}
}

func TestContainerTerminalCloseDoesNotHidePermissionOrDaemonErrors(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_CONTEXT", "")
	failure := filepath.Join(t.TempDir(), "failure")
	t.Setenv("TERMINAL_FAIL_CLOSE", failure)
	installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; test "$1" = owned-container; shift; if [ -f "$TERMINAL_FAIL_CLOSE" ]; then cat "$TERMINAL_FAIL_CLOSE" >&2; exit 42; fi; exec "$@"`)
	argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := argv[11]
	t.Cleanup(func() { _ = os.Remove(failure); _ = closeSession(context.Background()) })
	for _, message := range []string{"permission denied", "no such host", "Cannot connect to daemon"} {
		if err = os.WriteFile(failure, []byte(message), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = closeSession(context.Background()); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("runtime failure hidden: %v", err)
		}
		if _, err = os.Stat(directory); err != nil {
			t.Fatalf("failed close lost ownership: %v", err)
		}
	}
	_ = os.Remove(failure)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = closeSession(canceled); err == nil {
		t.Fatal("canceled close reported success")
	}
	if err = closeSession(context.Background()); err != nil {
		t.Fatalf("fresh close did not reclaim: %v", err)
	}
}

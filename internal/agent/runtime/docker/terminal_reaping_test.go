//go:build linux

package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper is the actual remote program. Each parent waits for its child and
// remains alive afterwards, so cleanup must let it reap before killing it.
func TestTerminalReapingHelper(t *testing.T) {
	if os.Getenv("GPUWORKS_TERMINAL_REAP_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGHUP)
	depth, err := strconv.Atoi(os.Getenv("GPUWORKS_TERMINAL_REAP_DEPTH"))
	if err != nil {
		t.Fatal(err)
	}
	var child *exec.Cmd
	if depth == 0 {
		child = exec.Command("sleep", "300")
	} else {
		child = exec.Command(os.Args[0], "-test.run=^TestTerminalReapingHelper$")
		child.Env = append(os.Environ(), "GPUWORKS_TERMINAL_REAP_DEPTH="+strconv.Itoa(depth-1))
	}
	switch os.Getenv("GPUWORKS_TERMINAL_REAP_GROUP") {
	case "separate":
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	case "session":
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("GPUWORKS_TERMINAL_REAP_DIRECTORY"), "child-"+strconv.Itoa(depth))
	if err = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	// The harness releases this barrier only after observing a dead child.
	// This models a cooperative parent that is not scheduled to wait in the
	// same instant as its child dies, without relying on a fixed delay.
	gate := path + "-allow-reap"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(gate); err == nil {
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("reap barrier never released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = child.Wait()
	if err = os.WriteFile(path+"-reaped", []byte("wait completed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestTerminalCloseWaitsForParentsToReapBeforeAscending(t *testing.T) {
	if os.Getenv("GPUWORKS_TERMINAL_SUBREAPER") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestTerminalCloseWaitsForParentsToReapBeforeAscending$", "-test.v")
		command.Env = append(os.Environ(), "GPUWORKS_TERMINAL_SUBREAPER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated PID1/no-reaper model: %v\n%s", err, output)
		}
		return
	}
	// PR_SET_CHILD_SUBREAPER keeps orphan zombies observable instead of allowing
	// the host init to hide the bug. Isolation avoids changing the test runner.
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); errno != 0 {
		t.Fatal(errno)
	}
	for _, group := range []string{"same", "separate", "session"} {
		for _, depth := range []int{0, 1} {
			t.Run(fmt.Sprintf("group=%s/depth=%d", group, depth), func(t *testing.T) {
				sock := shortUnixSocketPath(t)
				listener, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				t.Setenv("DOCKER_HOST", "unix://"+sock)
				t.Setenv("DOCKER_CONTEXT", "")
				t.Setenv("GPUWORKS_TERMINAL_REAP_HELPER", "1")
				t.Setenv("GPUWORKS_TERMINAL_REAP_DEPTH", strconv.Itoa(depth))
				t.Setenv("GPUWORKS_TERMINAL_REAP_GROUP", group)
				directory := t.TempDir()
				t.Setenv("GPUWORKS_TERMINAL_REAP_DIRECTORY", directory)
				installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; if [ "$1" = -i ]; then shift 2; fi; test "$1" = owned-container; shift; exec "$@"`)
				argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", []string{os.Args[0], "-test.run=^TestTerminalReapingHelper$"})
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
				var children []int
				starts := map[int]string{}
				defer func() {
					_ = input.Close()
					_ = command.Process.Kill()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("owned root did not exit during harness cleanup")
					}
					for _, pid := range children {
						raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
						if len(raw) > 0 {
							fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
							if len(fields) >= 20 && fields[19] == starts[pid] {
								_ = syscall.Kill(pid, syscall.SIGKILL)
							}
						}
						deadline := time.Now().Add(time.Second)
						for time.Now().Before(deadline) {
							var status syscall.WaitStatus
							_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
							if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
								break
							}
							time.Sleep(10 * time.Millisecond)
						}
					}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if err := closeSession(ctx); err != nil {
						t.Errorf("scoped harness cleanup: %v", err)
					}
				}()
				deadline := time.Now().Add(3 * time.Second)
				for index := depth; index >= 0; index-- {
					path := filepath.Join(directory, "child-"+strconv.Itoa(index))
					for {
						raw, err := os.ReadFile(path)
						if err == nil && len(raw) > 0 {
							pid, err := strconv.Atoi(string(raw))
							if err != nil || pid <= 1 {
								t.Fatalf("child identity: %q/%v", raw, err)
							}
							children = append(children, pid)
							observed, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
							if statErr != nil {
								t.Fatal(statErr)
							}
							identity := strings.Fields(string(observed[strings.LastIndexByte(string(observed), ')')+1:]))
							if len(identity) < 20 {
								t.Fatal("malformed fixture process stat")
							}
							starts[pid] = identity[19]
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("owned child not ready")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", children[0]))
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
				if (group != "session" && fields[3] != strconv.Itoa(command.Process.Pid)) || (group == "separate" && fields[2] == fields[3]) || (group == "session" && fields[3] == strconv.Itoa(command.Process.Pid)) {
					t.Fatalf("fixture session/group mismatch: %s", raw)
				}
				stopMonitor, monitorDone := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(monitorDone)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stopMonitor:
							return
						case <-ticker.C:
						}
						for index, pid := range children {
							raw, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
							if readErr != nil {
								continue
							}
							fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
							if len(fields) >= 20 && fields[0] == "Z" && fields[19] == starts[pid] {
								_ = os.WriteFile(filepath.Join(directory, "child-"+strconv.Itoa(depth-index)+"-allow-reap"), nil, 0o600)
							}
						}
					}
				}()
				defer func() { close(stopMonitor); <-monitorDone }()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err = closeSession(ctx); err != nil {
					for _, pid := range children {
						raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
						t.Logf("owned child before any external Wait4: %s", raw)
					}
					t.Fatalf("close must let parents reap children before terminating ancestors: %v", err)
				}
				// No external Wait4 has run yet: only the actual parent may have reaped.
				for _, pid := range children {
					if _, err = os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); !os.IsNotExist(err) {
						t.Fatalf("owned child %d not gone: %v", pid, err)
					}
				}
			})
		}
	}
}

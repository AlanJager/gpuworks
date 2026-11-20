//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func pinInstallFixtureCleanup(t *testing.T, req clusterInstallNodeRequest, kind string) clusterInstallProcessOwner {
	t.Helper()
	raw, err := clusterInstallReadPrivate(req.Dir + "/" + kind + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	var record clusterInstallPID
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	owner, err := clusterInstallReadOwner(req, kind, record)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{owner.PID, owner.Supervisor.PID} {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			_ = unix.Close(fd)
		})
	}
	return owner
}

func waitInstallFixtureReady(t *testing.T, req clusterInstallNodeRequest) {
	t.Helper()
	pid, err := clusterInstallOwnedProcess(req, "serve")
	if err != nil || pid == 0 {
		t.Fatalf("caller did not leave an owned server: %d %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(req.Dir, "logs", fmt.Sprintf("ready-%d", pid))); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("owned fixture did not become signal ready")
}

func TestClusterInstallCapturesRealExitAfterInvokingCallerExits(t *testing.T) {
	requirePidfd(t)
	for _, tc := range []struct{ mode, want string }{
		{"", ""}, {"7", "exit status 7"}, {"134", "exit status 134"},
		{"abort", "signal 6"}, {"kill", "signal 9"}, {"default-term", "signal 15"},
	} {
		name := tc.mode
		if name == "" {
			name = "clean"
		}
		t.Run(name, func(t *testing.T) {
			req := processInstallFixture(t)
			t.Setenv("CLUSTER_INSTALL_TEST_EXIT", tc.mode)
			raw, _ := json.Marshal(req)
			caller := exec.Command(req.Dir+"/gpuworks", "fixture-start")
			caller.Stdin = bytes.NewReader(raw)
			if output, err := caller.CombinedOutput(); err != nil {
				logs, _ := os.ReadFile(req.Dir + "/logs/process.out")
				t.Fatalf("separate start caller failed: %v %s; supervisor log: %s", err, output, logs)
			}
			waitInstallFixtureReady(t, req)
			owner := pinInstallFixtureCleanup(t, req, "serve")
			if _, alive, err := clusterInstallProcessIdentity(caller.Process.Pid); err != nil || alive {
				t.Fatal("original start caller is still alive")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := clusterInstallStop(ctx, req, "serve")
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("stop=%v, want %q", err, tc.want)
			}
			if _, alive, err := clusterInstallProcessIdentity(owner.Supervisor.PID); err != nil || alive {
				t.Fatal("stop completed before its owned parent exited")
			}
		})
	}
}

func TestClusterInstallAbnormalExitCannotCreateOrActivateService(t *testing.T) {
	requirePidfd(t)
	for _, mode := range []string{"7", "abort"} {
		t.Run(mode, func(t *testing.T) {
			req := processInstallFixture(t)
			t.Setenv("CLUSTER_INSTALL_TEST_EXIT", mode)
			if err := startInstallFixture(t, req, false, nil); err != nil {
				t.Fatal(err)
			}
			pinInstallFixtureCleanup(t, req, "serve")
			bin := t.TempDir()
			events := bin + "/events"
			script := "#!/bin/sh\nprintf invoked > \"$CLUSTER_INSTALL_TEST_EVENTS\"\nexit 99\n"
			if err := os.WriteFile(bin+"/sudo", []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			t.Setenv("CLUSTER_INSTALL_TEST_EVENTS", events)
			if err := clusterInstallService(t.Context(), req); err == nil {
				t.Fatal("abnormal detached exit permitted service handoff")
			}
			if _, err := os.Stat(events); !os.IsNotExist(err) {
				t.Fatal("service publication/activation ran after failed stop")
			}
		})
	}
}

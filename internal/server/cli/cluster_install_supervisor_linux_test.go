//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Drive the real executable's inherited-pipe protocol without acknowledging the
// final decision. This represents a caller that dies after reading ownership.
func TestClusterInstallCallerEOFAbortsUncommittedChild(t *testing.T) {
	requirePidfd(t)
	req := processInstallFixture(t)
	jobR, jobW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	decisionR, decisionW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{jobR, jobW, ackR, ackW, decisionR, decisionW} {
		defer f.Close()
	}
	c := exec.Command(req.Dir+"/gpuworks", clusterInstallSupervisorCommand)
	c.ExtraFiles = []*os.File{jobR, ackW, decisionR}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	t.Cleanup(func() { _ = c.Process.Kill() })
	jobR.Close()
	ackW.Close()
	decisionR.Close()
	launch := clusterInstallLaunch{Request: req, Kind: "serve", Args: []string{"serve", "--data", req.Dir + "/data"}}
	if err := json.NewEncoder(jobW).Encode(launch); err != nil {
		t.Fatal(err)
	}
	jobW.Close()
	var owner clusterInstallProcessOwner
	if err := json.NewDecoder(ackR).Decode(&owner); err != nil {
		t.Fatal(err)
	}
	pinInstallFixtureCleanup(t, req, "serve")
	// These three inherited pipes must never reach the server child.
	for _, f := range []*os.File{ackR, decisionW} {
		pipe, _ := os.Readlink("/proc/self/fd/" + fileDescriptor(f))
		entries, _ := os.ReadDir("/proc/" + processNumber(owner.PID) + "/fd")
		for _, entry := range entries {
			link, _ := os.Readlink("/proc/" + processNumber(owner.PID) + "/fd/" + entry.Name())
			if pipe != "" && link == pipe {
				t.Fatal("bootstrap pipe inherited by child")
			}
		}
	}
	decisionW.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("uncommitted launch accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uncommitted supervisor failed to reap child")
	}
	if _, alive, err := clusterInstallProcessIdentity(owner.PID); err != nil || alive {
		t.Fatal("uncommitted child survived caller EOF")
	}
	complete, err := clusterInstallReadExit(req, owner)
	if !complete || err == nil || !strings.Contains(err.Error(), "signal 9") {
		t.Fatalf("abort wait status=%v,%v", complete, err)
	}
}

func TestClusterInstallKilledSupervisorCannotProveChildExit(t *testing.T) {
	requirePidfd(t)
	req := processInstallFixture(t)
	if err := startInstallFixture(t, req, false, nil); err != nil {
		t.Fatal(err)
	}
	owner := pinInstallFixtureCleanup(t, req, "serve")
	fd, err := unix.PidfdOpen(owner.Supervisor.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 5000); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := clusterInstallStop(ctx, req, "serve"); err == nil || !strings.Contains(err.Error(), "without a terminal wait status") {
		t.Fatalf("unknown exit accepted: %v", err)
	}
}

func TestClusterInstallTerminalDirectorySyncFailureIsNotSuccess(t *testing.T) {
	req := processInstallFixture(t)
	if err := os.Mkdir(req.Dir+"/processes", 0700); err != nil {
		t.Fatal(err)
	}
	boot, _ := clusterInstallBootID()
	hash, _ := clusterInstallExecutableHash(req)
	owner := clusterInstallProcessOwner{clusterInstallPID: clusterInstallPID{999999999, "1"}, Supervisor: clusterInstallPID{999999998, "1"}, Boot: boot, Nonce: strings.Repeat("a", 32), Kind: "serve", ExecutableHash: hash}
	if err := clusterInstallPublishOwner(req, owner); err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	syncErr := errors.New("injected terminal directory fsync failure")
	err := clusterInstallPublishExit(req, clusterInstallProcessExit{Owner: owner, Status: &zero}, func(string) error { return syncErr })
	if !errors.Is(err, syncErr) {
		t.Fatalf("lost directory fsync error: %v", err)
	}
	if _, err := os.Stat(clusterInstallOwnerPath(req, owner.Nonce, "exit")); err != nil {
		t.Fatal("fixture did not reach visible rename")
	}
	if complete, err := clusterInstallReadExit(req, owner); complete || err == nil {
		t.Fatalf("failed durable publication accepted: %v,%v", complete, err)
	}
	if err := clusterInstallPreviousExits(req); err == nil {
		t.Fatal("incomplete publication allowed replacement")
	}
}

func TestClusterInstallAlreadyCanceledStartLeavesNoProcess(t *testing.T) {
	req := processInstallFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := clusterInstallStartContext(ctx, req, false, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(req.Dir + "/serve.pid"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled caller published ownership")
	}
}

// Keep these conversions test-only, without introducing any production IDs.
func fileDescriptor(f *os.File) string { return processNumber(int(f.Fd())) }
func processNumber(n int) string       { return fmt.Sprint(n) }

func TestClusterInstallCallerCancellationDuringOwnershipKeepsEveryChildRecorded(t *testing.T) {
	req := processInstallFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			owners, _ := filepath.Glob(req.Dir + "/processes/*.owner.json")
			if len(owners) > 0 {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	err := clusterInstallStartContext(ctx, req, false, nil)
	<-watched
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ownership cancellation was lost: %v", err)
	}
	// An accepted commit racing cancellation may retain the durable owned runtime.
	// Before commit, EOF must terminate and reap it. Either result must preserve a
	// valid owner and PID, never an unrecorded process or a successful Start.
	var raw []byte
	var readErr error
	publicationDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(publicationDeadline) {
		raw, readErr = clusterInstallReadPrivate(req.Dir + "/serve.pid")
		if !errors.Is(readErr, os.ErrNotExist) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var record clusterInstallPID
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	owner, err := clusterInstallReadOwner(req, "serve", record)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(owner.PID, 0)
	if err == nil {
		t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
	}
	parentFD, err := unix.PidfdOpen(owner.Supervisor.PID, 0)
	if err == nil {
		t.Cleanup(func() { _ = unix.PidfdSendSignal(parentFD, unix.SIGKILL, nil, 0); _ = unix.Close(parentFD) })
	}
	limit := time.Now().Add(5 * time.Second)
	for time.Now().Before(limit) {
		complete, exitErr := clusterInstallReadExit(req, owner)
		if complete {
			if exitErr == nil || !strings.Contains(exitErr.Error(), "signal 9") {
				t.Fatal("uncommitted child was not killed/reaped")
			}
			return
		}
		if exitErr != nil {
			t.Fatal(exitErr)
		}
		time.Sleep(time.Millisecond)
	}
	// A runtime retained after the one-byte commit remains pinned and fully owned.
	pid, err := clusterInstallOwnedProcess(req, "serve")
	if err != nil || pid != owner.PID {
		t.Fatalf("canceled caller left an untracked process: %d %v", pid, err)
	}
}

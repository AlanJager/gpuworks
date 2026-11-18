package cli

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServerLockKeepsOneServerAndNamesIt(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	release, err := takeServerLock(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(data, serverLockFile))
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file = %q", raw)
	}
	start := time.Now()
	if _, err := takeServerLock(data, 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "another gpuworks server (pid "+strconv.Itoa(os.Getpid())+") is running") {
		t.Fatalf("second server = %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("the second server did not wait for the lock")
	}
	if running, pid := serverRunning(data); !running || pid != os.Getpid() {
		t.Fatalf("serverRunning = %t %d", running, pid)
	}
	release()
	if running, _ := serverRunning(data); running {
		t.Fatal("a released lock reads as running")
	}
	// A maintenance command holding the lock delays a starting server, which
	// gets it once the command is done.
	stop, err := requireServerStopped(data)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); stop() }()
	release, err = takeServerLock(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// TestServerLockHelperProcess holds the lock for its parent test until it is
// killed.
func TestServerLockHelperProcess(t *testing.T) {
	data := os.Getenv("GPUWORKS_TEST_SERVER_LOCK")
	if data == "" {
		return // only a helper process for TestServerLockGoesWithACrashedServer
	}
	if _, err := takeServerLock(data, 0); err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	select {}
}

func TestServerLockGoesWithACrashedServer(t *testing.T) {
	data := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerLockHelperProcess$")
	cmd.Env = append(os.Environ(), "GPUWORKS_TEST_SERVER_LOCK="+data)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper said %q", line)
	}
	_, err = requireServerStopped(data)
	var running *serverRunningError
	if !errors.As(err, &running) || running.PID != cmd.Process.Pid {
		t.Fatalf("with the server up = %v", err)
	}
	// SIGKILL: the process runs no cleanup, and the kernel drops the lock.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	release, err := requireServerStopped(data)
	if err != nil {
		t.Fatalf("after the crash = %v", err)
	}
	release()
}

// serve holds the lock for its whole life: a second server on the same data
// directory refuses after the wait and says which process holds it, and a
// server that exits lets it go.
func TestServeHoldsTheServerLockForItsLife(t *testing.T) {
	saved := serverLockWait
	serverLockWait = 200 * time.Millisecond
	t.Cleanup(func() { serverLockWait = saved })
	dir := t.TempDir()
	data, logFile := filepath.Join(dir, "data"), filepath.Join(dir, "server.log")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	serveArgs := func(addr string) []string {
		return []string{"serve", "--addr", addr, "--data", data, "--log-file", logFile, "--recovery-enabled=false"}
	}
	release, err := takeServerLock(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if code := Run(log, serveArgs("127.0.0.1:0"), io.Discard, Options{}); code != 1 {
		t.Fatalf("second server exit = %d", code)
	}
	raw, _ := os.ReadFile(logFile)
	if !strings.Contains(string(raw), "another gpuworks server (pid "+strconv.Itoa(os.Getpid())+") is running on "+data) {
		t.Fatalf("log does not name the running server:\n%s", raw)
	}
	release()

	// A server that fails after taking the lock (its port is taken) still
	// lets it go when it exits.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if code := Run(log, serveArgs(held.Addr().String()), io.Discard, Options{}); code != 1 {
		t.Fatalf("server on a taken port exit = %d", code)
	}
	if raw, _ := os.ReadFile(filepath.Join(data, serverLockFile)); strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("serve did not write its pid into the lock: %q", raw)
	}
	stop, err := requireServerStopped(data)
	if err != nil {
		t.Fatalf("after the server exited = %v", err)
	}
	stop()
}

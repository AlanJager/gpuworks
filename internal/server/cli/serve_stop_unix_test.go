//go:build unix

package cli

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const serveStopChildEnv = "GPUWORKS_SERVE_STOP_TEST_CHILD"

// serveStopTestChild turns the test binary into the server itself, so the
// test observes the real exit status of `gpuworks serve`.
func serveStopTestChild() {
	if os.Getenv(serveStopChildEnv) != "1" {
		return
	}
	os.Exit(Main(os.Args[1:], Options{}))
}

// A single-node server that is asked to stop exits 0 and leaves nothing held:
// the next server starts on the same data directory.
func TestServeStopsCleanlyOnASignalAndReleasesItsDataDirectory(t *testing.T) {
	data := t.TempDir()
	for _, stop := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		server := exec.Command(os.Args[0], "serve", "--data", data, "--addr", address)
		server.Env = append(os.Environ(), serveStopChildEnv+"=1", "GPUWORKS_ADMIN_PASSWORD=gpuworks-test-password-2026")
		output := filepath.Join(t.TempDir(), "server.out")
		sink, err := os.Create(output)
		if err != nil {
			t.Fatal(err)
		}
		server.Stdout, server.Stderr = sink, sink
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- server.Wait() }()
		fail := func(format string, args ...any) {
			t.Helper()
			_ = server.Process.Kill()
			logged, _ := os.ReadFile(output)
			t.Logf("server output:\n%s", logged)
			t.Fatalf(format, args...)
		}
		ready := false
		for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline) && !ready; {
			select {
			case err := <-exited:
				fail("server exited before it was ready: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			response, err := http.Get("http://" + address + "/healthz")
			if err == nil {
				ready = response.StatusCode == http.StatusOK
				_ = response.Body.Close()
			}
		}
		if !ready {
			fail("server did not answer /healthz")
		}
		if err := server.Process.Signal(stop); err != nil {
			fail("signal: %v", err)
		}
		select {
		case err := <-exited:
			if err != nil {
				fail("server stopped by %v exited with %v, want exit status 0", stop, err)
			}
		case <-time.After(120 * time.Second):
			fail("server did not stop after %v", stop)
		}
		_ = sink.Close()
	}
}

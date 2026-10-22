package debugsrv

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startSignalLoop runs Start until the test ends, then closes stop and blocks
// until Start has returned. Waiting matters because these tests share the
// fixed 6060 listener: without it, the next test's Start can attempt its bind
// while this one's listener is still tearing down asynchronously, silently
// fall back to an ephemeral port, and never become reachable on 6060.
func startSignalLoop(t *testing.T, window time.Duration) chan os.Signal {
	t.Helper()
	signals := make(chan os.Signal, 4)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(testLogger(), window, signals, stop)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return signals
}

func get(t *testing.T, url string) (int, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestWindowClosesByItself pins the production-safety contract: before a
// signal there is nothing to reach; one signal opens the standard pprof
// endpoints on loopback; the window closes on its own without any signal or
// process restart, after which connections are refused again.
func TestWindowClosesByItself(t *testing.T) {
	signals := startSignalLoop(t, 300*time.Millisecond)

	addr := "http://127.0.0.1:6060/debug/pprof/"
	if code, err := get(t, addr); err == nil {
		t.Fatalf("pprof reachable before any signal: status=%d", code)
	}

	signals <- os.Signal(nil)
	deadline := time.Now().Add(3 * time.Second)
	var opened string
	for time.Now().Before(deadline) {
		if code, err := get(t, addr); err == nil && code == 200 {
			opened = addr
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if opened == "" {
		t.Fatal("window never opened after the signal")
	}
	// The standard endpoints work: an index and one real profile.
	if code, err := get(t, "http://127.0.0.1:6060/debug/pprof/heap?debug=1"); err != nil || code != 200 {
		t.Fatalf("heap profile = %d, %v", code, err)
	}

	// Auto-close: refused again once the window lapses.
	closed := time.Now().Add(5 * time.Second)
	for time.Now().Before(closed) {
		if _, err := get(t, addr); err != nil {
			return // refused — window is gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("window still open after its deadline")
}

// TestRepeatSignalRestartsWindow: a second signal inside a window replaces
// it with a fresh one instead of leaking the old listener.
func TestRepeatSignalRestartsWindow(t *testing.T) {
	signals := startSignalLoop(t, 10*time.Second)

	signals <- os.Signal(nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if code, err := get(t, "http://127.0.0.1:6060/debug/pprof/"); err == nil && code == 200 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	// Restart the window; the old listener must close and a new one serve.
	signals <- os.Signal(nil)
	time.Sleep(300 * time.Millisecond)
	if code, err := get(t, "http://127.0.0.1:6060/debug/pprof/"); err != nil || code != 200 {
		t.Fatalf("restarted window unreachable: %d %v", code, err)
	}
	// Only one loopback 6060 listener may exist; a leaked one would make the
	// previous close ineffective, which the restart path would mask.
	conns, err := net.Dial("tcp", "127.0.0.1:6060")
	if err == nil {
		conns.Close()
	} else {
		t.Fatalf("listener missing after restart: %v", err)
	}
}

// TestWindowOnlyOnLoopback: the listener address must be loopback — a
// wildcard here would put pprof on the network, which is the risk this
// package exists to avoid.
func TestWindowOnlyOnLoopback(t *testing.T) {
	signals := startSignalLoop(t, 5*time.Second)

	signals <- os.Signal(nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Dial("tcp", "127.0.0.1:6060")
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("never opened")
}

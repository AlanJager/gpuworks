//go:build linux || darwin

package terminal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Capture the actual wire and latch any exit written while cleanup is still
// pending. This detects early success even if reader scheduling hides its
// arrival until after the cleanup callback finishes.
type cleanupWire struct {
	input     io.Reader
	mu        sync.Mutex
	output    bytes.Buffer
	finished  *atomic.Bool
	earlyExit bool
}

func (w *cleanupWire) Read(p []byte) (int, error) { return w.input.Read(p) }
func (w *cleanupWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.output.Write(p)
	frames := proto.NewTermFrameReader(bytes.NewReader(w.output.Bytes()))
	for {
		frame, readErr := frames.Next()
		if readErr != nil {
			break
		}
		if frame.Type == proto.TermExit && !w.finished.Load() {
			w.earlyExit = true
		}
	}
	return n, err
}
func (w *cleanupWire) snapshot() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.output.Bytes()...), w.earlyExit
}
func cleanupExitCodes(t *testing.T, raw []byte) ([]int32, []string) {
	t.Helper()
	var codes []int32
	var reasons []string
	frames := proto.NewTermFrameReader(bytes.NewReader(raw))
	for {
		frame, err := frames.Next()
		if err == io.EOF {
			return codes, reasons
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == proto.TermExit {
			code, reason, err := proto.DecodeExit(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			codes = append(codes, code)
			reasons = append(reasons, reason)
		}
	}
}

func TestServeCommitsOnlyTheOutcomeAfterOwnedCleanup(t *testing.T) {
	for _, name := range []string{"normal", "nonzero-shell", "close-error", "open-error", "open-and-close-error", "disconnect", "disconnect-close-error", "host-nil"} {
		t.Run(name, func(t *testing.T) {
			reader, input := io.Pipe()
			defer reader.Close()
			defer input.Close()
			var finished atomic.Bool
			wire := &cleanupWire{input: reader, finished: &finished}
			var calls atomic.Int32
			failure := errors.New("remote cleanup denied")
			spec := Spec{Argv: []string{"/bin/sh", "-c", "exit 0"}, Rows: 24, Cols: 80, Grace: 100 * time.Millisecond}
			spec.Cleanup = func() error {
				calls.Add(1)
				defer finished.Store(true)
				if strings.Contains(name, "close-error") {
					return failure
				}
				return nil
			}
			switch name {
			case "nonzero-shell":
				spec.Argv = []string{"/bin/sh", "-c", "exit 7"}
			case "open-error", "open-and-close-error":
				spec.Argv = []string{"/missing-owned-terminal"}
			case "host-nil":
				spec.Cleanup = nil
				finished.Store(true)
			}
			disconnected := strings.HasPrefix(name, "disconnect")
			if disconnected {
				spec.Argv = []string{"/bin/sh", "-c", "read line"}
				_ = input.Close()
			}
			err := Serve(wire, spec)
			raw, early := wire.snapshot()
			codes, reasons := cleanupExitCodes(t, raw)
			if early {
				t.Fatalf("terminal outcome preceded owned cleanup: %v", codes)
			}
			expectedCalls := int32(1)
			if name == "host-nil" {
				expectedCalls = 0
			}
			if calls.Load() != expectedCalls {
				t.Fatalf("cleanup calls=%d, want %d", calls.Load(), expectedCalls)
			}
			if strings.Contains(name, "close-error") {
				if !errors.Is(err, failure) {
					t.Fatalf("cleanup failure hidden: %v", err)
				}
			} else if strings.Contains(name, "open-error") {
				if err == nil {
					t.Fatal("open failure hidden")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if disconnected {
				if len(codes) != 0 {
					t.Fatalf("closed peer was sent an outcome: %v", codes)
				}
				return
			}
			if len(codes) != 1 {
				t.Fatalf("terminal must emit one outcome: %v", codes)
			}
			if strings.Contains(name, "close-error") || strings.Contains(name, "open-error") {
				if codes[0] != -1 {
					t.Fatalf("failed lifecycle reported success: %v", codes)
				}
				if strings.Contains(name, "close-error") && !strings.Contains(reasons[0], failure.Error()) {
					t.Fatalf("cleanup failure absent from wire: %q", reasons[0])
				}
			} else {
				want := int32(0)
				if name == "nonzero-shell" {
					want = 7
				}
				if codes[0] != want {
					t.Fatalf("shell outcome changed: %v", codes)
				}
			}
		})
	}
}

func TestServeKeepsOutcomeUncommittedDuringHeldCleanup(t *testing.T) {
	reader, input := io.Pipe()
	defer reader.Close()
	defer input.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var finished atomic.Bool
	wire := &cleanupWire{input: reader, finished: &finished}
	spec := Spec{Argv: []string{"/bin/sh", "-c", "exit 0"}, Rows: 24, Cols: 80, Cleanup: func() error { close(entered); <-release; finished.Store(true); return nil }}
	result := make(chan error, 1)
	go func() { result <- Serve(wire, spec) }()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("Serve ended before cleanup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup never started")
	}
	if raw, early := wire.snapshot(); early {
		t.Fatalf("exit was already committed: %x", raw)
	} else {
		codes, _ := cleanupExitCodes(t, raw)
		if len(codes) != 0 {
			t.Fatalf("held cleanup already has outcome: %v", codes)
		}
	}
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not finish after cleanup")
	}
	raw, early := wire.snapshot()
	codes, _ := cleanupExitCodes(t, raw)
	if early || len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("invalid committed outcome: early=%v codes=%v", early, codes)
	}
}

func TestServeKeepsLocalOwnerAliveUntilRuntimeCleanup(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(cleanupFails), func(t *testing.T) {
			reader, input := io.Pipe()
			defer reader.Close()
			defer input.Close()
			var finished atomic.Bool
			wire := &cleanupWire{input: reader, finished: &finished}
			pidFile := filepath.Join(t.TempDir(), "terminal.pid")
			var pid int
			var ownerAlive atomic.Bool
			cleanupFailure := errors.New("owned runtime cleanup failed")
			result := make(chan error, 1)
			go func() {
				result <- Serve(wire, Spec{
					Argv:  []string{"/bin/sh", "-c", `printf '%s' "$$" > "$1"; read line`, "terminal-test", pidFile},
					Grace: 100 * time.Millisecond,
					Cleanup: func() error {
						ownerAlive.Store(syscall.Kill(pid, 0) == nil)
						finished.Store(true)
						if cleanupFails {
							return cleanupFailure
						}
						return nil
					},
				})
			}()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for pid == 0 {
				if raw, err := os.ReadFile(pidFile); err == nil {
					pid, _ = strconv.Atoi(string(raw))
				}
				if pid > 0 {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("terminal ended before disconnect: %v", err)
				case <-deadline.C:
					t.Fatal("terminal did not record its owner")
				case <-tick.C:
				}
			}
			// Closing the peer releases the input pump, which starts teardown.
			_ = input.Close()
			select {
			case err := <-result:
				if cleanupFails && !errors.Is(err, cleanupFailure) {
					t.Fatalf("runtime cleanup failure hidden: %v", err)
				}
				if !cleanupFails && err != nil {
					t.Fatal(err)
				}
			case <-deadline.C:
				t.Fatal("terminal did not finish after disconnect")
			}
			if !ownerAlive.Load() {
				t.Error("local owner was reaped before runtime cleanup could inspect its descendants")
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("local owner survived teardown: %v", err)
			}
		})
	}
}

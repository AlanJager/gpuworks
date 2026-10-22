package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCLI installs an executable script named name first on PATH.
func fakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func shortVendorCLITimeouts(t *testing.T, timeout, grace time.Duration) {
	t.Helper()
	oldTimeout, oldGrace := vendorCLITimeout, vendorCLIReapGrace
	vendorCLITimeout, vendorCLIReapGrace = timeout, grace
	t.Cleanup(func() { vendorCLITimeout, vendorCLIReapGrace = oldTimeout, oldGrace })
}

func TestRunVendorCLIReturnsCombinedOutputAndExitError(t *testing.T) {
	fakeCLI(t, "gw-fake-smi", "echo out; echo err >&2; read line; echo \"in=$line\"; exit ${1:-0}\n")
	out, err := RunVendorCLI(t.Context(), strings.NewReader("Y\n"), "gw-fake-smi")
	if err != nil || out != "out\nerr\nin=Y\n" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	out, err = RunVendorCLI(t.Context(), nil, "gw-fake-smi", "9")
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 9 || IsCLIUnresponsive(err) || !strings.Contains(out, "out") {
		t.Fatalf("exit 9: output = %q, err = %v; want the exit error with output, not unresponsive", out, err)
	}
	if _, err := RunVendorCLI(t.Context(), nil, "gw-missing-smi"); err == nil || IsCLIUnresponsive(err) {
		t.Fatalf("missing binary err = %v", err)
	}
}

func TestRunVendorCLITimeoutIsUnresponsiveAndCallerCancelIsNot(t *testing.T) {
	shortVendorCLITimeouts(t, 200*time.Millisecond, 3*time.Second)
	fakeCLI(t, "gw-slow-smi", "exec sleep 5\n")
	start := time.Now()
	_, err := RunVendorCLI(t.Context(), nil, "gw-slow-smi")
	if !IsCLIUnresponsive(err) || !strings.Contains(err.Error(), "no answer within 200ms") {
		t.Fatalf("timeout err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timed-out call took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := RunVendorCLI(ctx, nil, "gw-slow-smi"); !errors.Is(err, context.Canceled) || IsCLIUnresponsive(err) {
		t.Fatalf("cancelled call err = %v, want context.Canceled only", err)
	}
}

// A CLI that does not exit after the kill (here: a child keeps the output
// pipe open, standing in for a process stuck in the driver) releases the
// caller after the grace period, and further calls fail at once without
// starting another process until it is gone.
func TestRunVendorCLIStuckProcessFailsFastUntilItExits(t *testing.T) {
	shortVendorCLITimeouts(t, 200*time.Millisecond, 50*time.Millisecond)
	dir := fakeCLI(t, "gw-stuck-smi", `echo x >> "$(dirname "$0")/starts"
if [ -e "$(dirname "$0")/healthy" ]; then echo ok; exit 0; fi
sleep 2 &
sleep 5
`)
	starts := func() int {
		raw, _ := os.ReadFile(filepath.Join(dir, "starts"))
		return strings.Count(string(raw), "x")
	}

	start := time.Now()
	if _, err := RunVendorCLI(t.Context(), nil, "gw-stuck-smi"); !IsCLIUnresponsive(err) {
		t.Fatalf("first call err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("stuck call held the caller for %s", elapsed)
	}
	if err := os.WriteFile(filepath.Join(dir, "healthy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RunVendorCLI(t.Context(), nil, "gw-stuck-smi")
	if !IsCLIUnresponsive(err) || !strings.Contains(err.Error(), "has not exited") || starts() != 1 {
		t.Fatalf("call while stuck: err = %v, starts = %d; want fail-fast without a new process", err, starts())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := RunVendorCLI(t.Context(), nil, "gw-stuck-smi")
		if err == nil {
			if out != "ok\n" {
				t.Fatalf("recovered output = %q", out)
			}
			break
		}
		if !IsCLIUnresponsive(err) || time.Now().After(deadline) {
			t.Fatalf("CLI never recovered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A timeout says nothing about which fields the driver accepts: Metrics must
// neither try the smaller tiers nor cache one, and Probe must not walk down
// its inventory queries.
func TestNVIDIAUnresponsiveCLIDoesNotDowngradeQueries(t *testing.T) {
	unresponsive := fmt.Errorf("nvidia-smi: no answer within 10s: %w", ErrCLIUnresponsive)
	var calls []string
	slow := true
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		if slow {
			return "", unresponsive
		}
		switch args[0] {
		case metricsQueryEventReasons:
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 12, 1400, 1980, 0x0000000000000000\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		}
		return "", errors.New("unexpected call: " + args[0])
	}}
	if _, err := p.Metrics(t.Context()); !IsCLIUnresponsive(err) {
		t.Fatalf("Metrics err = %v", err)
	}
	if len(calls) != 1 || calls[0] != metricsQueryEventReasons {
		t.Fatalf("Metrics calls while unresponsive = %v, want only the top tier", calls)
	}
	calls = nil
	if _, err := p.Probe(t.Context()); !IsCLIUnresponsive(err) {
		t.Fatalf("Probe err = %v", err)
	}
	if len(calls) != 1 || calls[0] != nvidiaInventoryQueries[0] {
		t.Fatalf("Probe calls while unresponsive = %v, want only the first inventory query", calls)
	}

	slow, calls = false, nil
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 1 || calls[0] != metricsQueryEventReasons {
		t.Fatalf("recovered Metrics = %+v, %v (calls %v); want the top tier", metrics, err, calls)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_bandwidth_utilization"]; !ok {
		t.Fatalf("recovered Metrics lost top-tier fields: %+v", metrics[0].Values)
	}
}

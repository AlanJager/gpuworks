package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHygonComputeUnitsDoesNotUseCachedSlowQuery(t *testing.T) {
	queryTimings(t, time.Second, 10*time.Second, time.Minute)
	dir := fakeCLI(t, "hy-smi", `d="$(dirname "$0")"
if [ -e "$d/slow" ]; then exec sleep 1; fi
sleep 0.1
cat <<'REPORT'
Device 0:
Physical Device: 0
Compute units: 128
REPORT
`)
	args := []string{"virtual", "-d", "0", "--show-device-info"}
	p := &HygonProber{}
	command, err := p.lookup("hy-smi")
	if err != nil {
		t.Fatal(err)
	}
	// Monitoring may use the command name or its resolved path as the cache
	// key. Prime both so either cached-query regression must be detected.
	for _, key := range []string{"hy-smi", command} {
		if _, err := QueryVendorCLI(t.Context(), key, args...); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, key)
	}
	// The earlier result remains available to monitoring even when the next
	// query hangs. Admission must still obtain a new physical-device answer.
	vendorCLITimeout = 30 * time.Millisecond
	if err := os.WriteFile(filepath.Join(dir, "slow"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if count, err := p.ComputeUnits(t.Context(), 0); err == nil || count != 0 {
		t.Fatalf("admission reused cached CUs after the CLI stopped answering: count=%d err=%v", count, err)
	}
}

func TestHygonComputeUnitsRunsVendorCLI(t *testing.T) {
	queryTimings(t, time.Second, 10*time.Second, time.Minute)
	raw, err := os.ReadFile("testdata/hy-smi-virtual-k100ai.txt")
	if err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, "hy-smi", `[ "$#" -eq 4 ] && [ "$1" = virtual ] && [ "$2" = -d ] && [ "$3" = 0 ] && [ "$4" = --show-device-info ] || exit 2
cat <<'REPORT'
`+string(raw)+"REPORT\n")
	if got, err := (&HygonProber{}).ComputeUnits(t.Context(), 0); err != nil || got != 128 {
		t.Fatalf("CLI physical CUs = %d, %v", got, err)
	}
}

func TestHygonComputeUnitsUsesResolvedVendorBinary(t *testing.T) {
	dir := fakeCLI(t, "hy-smi-resolved", "printf 'Device 2:\\nPhysical Device: 2\\nCompute units: 60\\n'\n")
	p := &HygonProber{lookPath: func(name string) (string, error) {
		if name != "hy-smi" {
			t.Fatalf("looked up %q", name)
		}
		return filepath.Join(dir, "hy-smi-resolved"), nil
	}}
	if got, err := p.ComputeUnits(t.Context(), 2); err != nil || got != 60 {
		t.Fatalf("resolved CLI physical CUs = %d, %v", got, err)
	}
	wantErr := errors.New("vendor CLI is missing")
	p.lookPath = func(string) (string, error) { return "", wantErr }
	if _, err := p.ComputeUnits(t.Context(), 2); !errors.Is(err, wantErr) {
		t.Fatalf("lookup error = %v, want %v", err, wantErr)
	}
}

func TestHygonComputeUnitsUsesPhysicalDeviceQuery(t *testing.T) {
	raw, err := os.ReadFile("testdata/hy-smi-virtual-k100ai.txt")
	if err != nil {
		t.Fatal(err)
	}
	p := &HygonProber{command: func(ctx context.Context, args ...string) (string, error) {
		want := []string{"virtual", "-d", "0", "--show-device-info"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("query arguments = %v, want %v", args, want)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("physical CU query has no deadline")
		}
		return string(raw), nil
	}}
	got, err := p.ComputeUnits(context.Background(), 0)
	if err != nil || got != 128 {
		t.Fatalf("K100AI physical CUs = %d, %v", got, err)
	}
	// The count comes from hardware rather than the model name or index.
	p.command = func(_ context.Context, args ...string) (string, error) {
		if args[2] != "3" {
			t.Fatalf("physical device argument = %q", args[2])
		}
		return "Device 3:\n\tPhysical Device: 3\n\tCompute units: 60\n", nil
	}
	if got, err := p.ComputeUnits(context.Background(), 3); err != nil || got != 60 {
		t.Fatalf("Z100 physical CUs = %d, %v", got, err)
	}
}

func TestHygonComputeUnitsRejectsUnverifiedReports(t *testing.T) {
	valid := "Device 0:\n\tPhysical Device: 0\n\tCompute units: 128\n"
	for name, raw := range map[string]string{
		"empty":            "",
		"missing count":    "Device 0:\nPhysical Device: 0\n",
		"missing identity": "Device 0:\nCompute units: 128\n",
		"wrong header":     strings.Replace(valid, "Device 0:", "Device 1:", 1),
		"wrong physical":   strings.Replace(valid, "Physical Device: 0", "Physical Device: 1", 1),
		"zero":             strings.Replace(valid, "128", "0", 1),
		"negative":         strings.Replace(valid, "128", "-1", 1),
		"malformed":        strings.Replace(valid, "128", "128 CUs", 1),
		"overflow":         strings.Replace(valid, "128", "999999999999999999999", 1),
		"duplicate count":  valid + "Compute units: 60\n",
		"duplicate device": valid + valid,
		"multiple devices": valid + "Device 1:\nPhysical Device: 1\nCompute units: 60\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := &HygonProber{command: func(context.Context, ...string) (string, error) { return raw, nil }}
			if got, err := p.ComputeUnits(context.Background(), 0); err == nil || got != 0 {
				t.Fatalf("unverified CUs = %d, %v", got, err)
			}
		})
	}
}

func TestHygonComputeUnitsPropagatesCommandFailureAndCancellation(t *testing.T) {
	wantErr := errors.New("driver unavailable")
	p := &HygonProber{command: func(context.Context, ...string) (string, error) { return "", wantErr }}
	if _, err := p.ComputeUnits(context.Background(), 0); !errors.Is(err, wantErr) {
		t.Fatalf("query error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.command = func(context.Context, ...string) (string, error) {
		t.Fatal("canceled request ran the command")
		return "", nil
	}
	if _, err := p.ComputeUnits(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query = %v", err)
	}
	if _, err := p.ComputeUnits(context.Background(), -1); err == nil {
		t.Fatal("negative physical index accepted")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	p.command = func(context.Context, ...string) (string, error) {
		cancel()
		return "Device 0:\nPhysical Device: 0\nCompute units: 128\n", nil
	}
	if _, err := p.ComputeUnits(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("query canceled while running = %v", err)
	}
}

package gpu

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func withDCGMSeams(t *testing.T, lookPath func(string) (string, error), runCommand func(context.Context, ...string) (string, error)) {
	t.Helper()
	origLookPath, origRun := dcgmLookPath, dcgmRunCommand
	t.Cleanup(func() { dcgmLookPath, dcgmRunCommand = origLookPath, origRun })
	if lookPath != nil {
		dcgmLookPath = lookPath
	}
	if runCommand != nil {
		dcgmRunCommand = runCommand
	}
}

// The default (empty mode, and the explicit "off") never even checks PATH:
// DCGM is opt-in, so a node that never passed --dcgm=auto must not pay any
// cost for it, not even a PATH lookup.
func TestNewDCGMProbeDefaultAndOffModeSkipPathLookup(t *testing.T) {
	for _, mode := range []string{"", "off", "OFF"} {
		called := false
		withDCGMSeams(t, func(string) (string, error) { called = true; return "/usr/bin/dcgmi", nil }, nil)
		if p := NewDCGMProbe(mode); p != nil {
			t.Fatalf("mode=%q must return nil, got %+v", mode, p)
		}
		if called {
			t.Fatalf("mode=%q must not even check PATH", mode)
		}
	}
}

// Any mode other than exactly "auto" (case-insensitively) disables the probe,
// even a typo or a future/unknown value — fail closed, not open.
func TestNewDCGMProbeUnknownModeDisables(t *testing.T) {
	withDCGMSeams(t, func(string) (string, error) { return "/usr/bin/dcgmi", nil }, nil)
	for _, mode := range []string{"on", "true", "1", "atuo"} {
		if p := NewDCGMProbe(mode); p != nil {
			t.Fatalf("mode=%q must return nil (only \"auto\" enables), got %+v", mode, p)
		}
	}
}

func TestNewDCGMProbeAutoAbsentBinaryReturnsNil(t *testing.T) {
	withDCGMSeams(t, func(string) (string, error) { return "", exec.ErrNotFound }, nil)
	for _, mode := range []string{"auto", "AUTO"} {
		if p := NewDCGMProbe(mode); p != nil {
			t.Fatalf("mode=%q with no dcgmi on PATH must return nil, got %+v", mode, p)
		}
	}
}

func TestNewDCGMProbeAutoAvailableReturnsProbe(t *testing.T) {
	withDCGMSeams(t, func(name string) (string, error) {
		if name != "dcgmi" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/dcgmi", nil
	}, nil)
	if p := NewDCGMProbe("auto"); p == nil {
		t.Fatal("mode=auto with dcgmi on PATH must return a non-nil probe")
	}
	if p := NewDCGMProbe("AUTO"); p == nil {
		t.Fatal("mode=AUTO (case-insensitive) with dcgmi on PATH must return a non-nil probe")
	}
}

func TestDCGMProbeSampleIssuesExactlyOneCommand(t *testing.T) {
	var calls [][]string
	withDCGMSeams(t, nil, func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "#Entity  SMACT  SMOCC  TENSO\nGPU 0  0.945  0.882  0.623\nGPU 1  0.010  0.005  0.000\n", nil
	})
	p := &DCGMProbe{}
	samples, err := p.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("dcgmRunCommand calls = %+v, want exactly 1", calls)
	}
	if calls[0][0] != "dmon" {
		t.Fatalf("call = %+v, want dmon first", calls[0])
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want 2 GPUs", samples)
	}
	if samples[0] != (DCGMSample{SMActive: 0.945, SMOccupancy: 0.882, TensorActive: 0.623}) {
		t.Fatalf("gpu 0 = %+v", samples[0])
	}
	if samples[1] != (DCGMSample{SMActive: 0.010, SMOccupancy: 0.005, TensorActive: 0.000}) {
		t.Fatalf("gpu 1 = %+v", samples[1])
	}
}

func TestDCGMProbeSampleSkipsUnsampledNARows(t *testing.T) {
	withDCGMSeams(t, nil, func(context.Context, ...string) (string, error) {
		return "#Entity  SMACT  SMOCC  TENSO\nGPU 0  N/A  N/A  N/A\nGPU 1  0.5  0.4  0.3\n", nil
	})
	p := &DCGMProbe{}
	samples, err := p.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample = %v", err)
	}
	if _, ok := samples[0]; ok {
		t.Fatalf("an N/A row must not produce a sample: %+v", samples)
	}
	if got, ok := samples[1]; !ok || got.SMActive != 0.5 {
		t.Fatalf("gpu 1 = %+v, ok=%v", got, ok)
	}
}

func TestDCGMProbeSampleCommandFailureReturnsError(t *testing.T) {
	withDCGMSeams(t, nil, func(context.Context, ...string) (string, error) {
		return "dcgmi: permission denied", errors.New("exit status 1")
	})
	p := &DCGMProbe{}
	samples, err := p.Sample(context.Background())
	if err == nil {
		t.Fatal("expected an error when the dcgmi command fails")
	}
	if samples != nil {
		t.Fatalf("samples on error = %+v, want nil", samples)
	}
}

func TestDCGMProbeSampleIgnoresHeaderAndBlankLines(t *testing.T) {
	withDCGMSeams(t, nil, func(context.Context, ...string) (string, error) {
		return "\n# Entity  SMACT  SMOCC  TENSO\n\nGPU 0  0.1  0.2  0.3\n\n", nil
	})
	samples, err := (&DCGMProbe{}).Sample(context.Background())
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples=%+v err=%v", samples, err)
	}
}

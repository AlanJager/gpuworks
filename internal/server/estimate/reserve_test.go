package estimate

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// declaredBase stands in for the amount an engine declares in its own profile
// (deploy.Profile.ReserveBase); its value is arbitrary here, because what this
// test pins is the estimator's behavior. The number SGLang actually declares is
// pinned in the deploy package, next to the profile that declares it.
const declaredBase int64 = 205 << 20

// The table is a set of measured choices, not a derivation, so it is pinned
// here: changing one of these numbers should be a deliberate edit in both
// places. Each want is the base in MiB, with the utilization term taken back
// out so the table is what is being checked.
func TestReserveTableIsTheMeasuredOne(t *testing.T) {
	tests := []struct {
		name        string
		reserveBase int64
		arch        string
		graphs      bool
		want        int64
	}{
		{"vLLM on Hopper, graphs on", 0, "Hopper", true, 4300},
		{"vLLM on Hopper, graphs off", 0, "Hopper", false, 1540},
		{"vLLM on Blackwell, graphs on", 0, "Blackwell", true, 4300},
		{"vLLM on Ampere, graphs on", 0, "Ampere", true, 1330},
		{"vLLM on Turing, graphs on", 0, "Turing", true, 1330},
		{"vLLM on Ampere, graphs off", 0, "Ampere", false, 410},
		{"a declared base ignores the architecture", declaredBase, "Hopper", true, 205},
		{"a declared base ignores the graph switch", declaredBase, "Ampere", false, 205},
		// Nothing measured these, so they must land on the larger branch rather
		// than the smaller one: an over-reserve costs KV room, an under-reserve
		// costs the deployment.
		{"an unmeasured architecture", 0, "Ascend910B", true, 4300},
		{"no architecture at all", 0, "", true, 4300},
		{"an architecture nobody has heard of", 0, "someday-arch", true, 4300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const mib = int64(1) << 20
			util := 0.9
			got := ReserveBytes(tt.reserveBase, tt.arch, tt.graphs, util) - int64(float64(shortfallBytes)*util)
			if got != tt.want*mib {
				t.Fatalf("base = %d MiB, want %d", got/mib, tt.want)
			}
		})
	}
}

// The two terms behave differently and that is the point of splitting them: the
// driver's shortfall is spent in proportion to utilization, the engine's own
// holdback is not. Hopper measured 4.20 GiB at both 0.5 and 0.9.
func TestOnlyTheShortfallMovesWithUtilization(t *testing.T) {
	short := float64(shortfallBytes)
	for _, base := range []int64{0, declaredBase} {
		lo := ReserveBytes(base, "Hopper", true, 0.5)
		hi := ReserveBytes(base, "Hopper", true, 0.9)
		want := int64(short * 0.4)
		if hi-lo != want {
			t.Fatalf("base %d moved by %d across utilizations, want only the shortfall's %d", base, hi-lo, want)
		}
	}
}

// The reserve charges the utilization it is given and substitutes nothing. A
// deployment's utilization is materialized at the write boundary, so a caller
// with none has a defect the scheduler rejects (scheduler.Request.Validate);
// quietly charging some other share here would hide it behind a number nobody
// chose.
func TestReserveChargesTheUtilizationItIsGiven(t *testing.T) {
	base := ReserveBytes(0, "Hopper", true, 0) // only the engine holdback
	for _, util := range []float64{0.5, 0.88, 0.92} {
		want := base + int64(float64(shortfallBytes)*util)
		if got := ReserveBytes(0, "Hopper", true, util); got != want {
			t.Fatalf("util=%v reserve = %d, want %d", util, got, want)
		}
	}
	if ReserveBytes(0, "Hopper", true, 0.88) == ReserveBytes(0, "Hopper", true, 0.92) {
		t.Fatal("two different utilizations must not be charged the same reserve")
	}
}

// On Apple silicon vllm-metal holds a flat amount: no CUDA graphs to size and
// no driver slice beyond reach, so neither the graph switch nor utilization
// moves it, and an engine's own declared holdback still wins.
func TestReserveOnAppleSiliconIsFlat(t *testing.T) {
	for _, family := range []string{"m1", "M4", " m5 "} {
		for _, graphs := range []bool{true, false} {
			for _, util := range []float64{0.5, 0.9} {
				if got := ReserveBytes(0, family, graphs, util); got != baseMetal {
					t.Errorf("ReserveBytes(%q, graphs=%v, util=%v) = %d, want %d", family, graphs, util, got, baseMetal)
				}
			}
		}
		if got := ReserveBytes(1<<30, family, true, 0.9); got != 1<<30 {
			t.Errorf("declared holdback on %q = %d", family, got)
		}
	}
	// Not Apple: names that only start with an m.
	for _, family := range []string{"maxwell", "m", "mi300", ""} {
		if accelerator.IsAppleFamily(family) {
			t.Errorf("IsAppleFamily(%q) = true", family)
		}
	}
}

// An NVIDIA slice is charged its limit plus the context HAMi cannot see and
// never asks for the whole-card reserve, whose ledger form reads the
// instance's deployment; a backend nobody measured is charged that reserve.
func TestPartitionClaimBytes(t *testing.T) {
	const limit = int64(8) << 30
	if got := PartitionClaimBytes(limit, accelerator.BackendNVIDIALogical, func() int64 { t.Fatal("the reserve was asked for an NVIDIA slice"); return 0 }); got != limit+512<<20 {
		t.Fatalf("nvidia claim = %d, want limit + 512 MiB", got)
	}
	if got := PartitionClaimBytes(limit, accelerator.BackendAscendLogical, func() int64 { return 2 << 30 }); got != limit+2<<30 {
		t.Fatalf("ascend claim = %d, want limit + reserve", got)
	}
}

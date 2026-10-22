package estimate

import "testing"

const gib int64 = 1 << 30

// The reserve is a promise about bytes, so it is checked in bytes: what the
// engine's share rounds down to, subtracted from the card. Checking it as a
// fraction instead would compare floats at the exact boundary (a 10 GiB card
// lands on 0.80, whose nearest float64 is a hair above it) and report a
// shortfall that does not exist in the memory anyone allocates.
func TestMaxGPUMemUtilAlwaysLeavesTheReserve(t *testing.T) {
	for _, total := range []int64{3 * gib, 4 * gib, 8 * gib, 10 * gib, 11 * gib, 12 * gib, 16 * gib, 20 * gib, 24 * gib, 48 * gib, 80 * gib} {
		util, ok := MaxGPUMemUtil(total, false, 0.01)
		if !ok {
			t.Fatalf("total=%d GiB reported no answer", total/gib)
		}
		left := total - int64(util*float64(total))
		if left < cardReserveBytes {
			t.Errorf("total=%d GiB util=%.2f leaves %d bytes, below the %d-byte reserve",
				total/gib, util, left, cardReserveBytes)
		}
	}
}

// The figures the deploy form will show. They are spelled out because the
// point of the rule is which number a deployer sees on their card, and a case
// that recomputed the formula would agree with any formula.
func TestMaxGPUMemUtilOnTheCardsThisChanges(t *testing.T) {
	for _, tc := range []struct {
		total int64
		want  float64
	}{
		{8 * gib, 0.75},  // 2080, 3070
		{10 * gib, 0.80}, // 3080 10G
		{11 * gib, 0.81}, // 2080 Ti
		{12 * gib, 0.83}, // 3080 12G, 3060
		{16 * gib, 0.87}, // T4, V100, A4000
		{20 * gib, 0.90}, // the size at which the reserve stops binding
		{24 * gib, 0.91}, // 4090, A10, L4
		{48 * gib, 0.95}, // Quadro RTX 8000, L40S
		{80 * gib, 0.97}, // A100, H100
	} {
		got, ok := MaxGPUMemUtil(tc.total, false, 0.01)
		if !ok || got != tc.want {
			t.Errorf("MaxGPUMemUtil(%d GiB) = %.4f (ok=%v), want %.2f", tc.total/gib, got, ok, tc.want)
		}
	}
}

// A unified-memory card's reserve is a share, so the answer does not depend on
// how large the pool is: the host needs more of a bigger machine.
func TestMaxGPUMemUtilIsAFractionOnUnifiedMemory(t *testing.T) {
	for _, total := range []int64{64 * gib, 128 * gib, 256 * gib} {
		got, ok := MaxGPUMemUtil(total, true, 0.01)
		if !ok || got != 0.8 {
			t.Errorf("MaxGPUMemUtil(%d GiB, unified) = %.4f (ok=%v), want 0.80", total/gib, got, ok)
		}
		left := total - int64(got*float64(total))
		if want := total / 5; left < want {
			t.Errorf("unified %d GiB leaves %d bytes, want at least %d", total/gib, left, want)
		}
	}
}

// A card too small to hold the reserve cannot be served at all, and saying so
// with a zero share is what lets admission refuse it for capacity. Inventing a
// share here would mean promising an engine memory the card does not have.
func TestMaxGPUMemUtilIsZeroWhenTheCardCannotHoldTheReserve(t *testing.T) {
	for _, total := range []int64{gib / 2, gib, 2 * gib} {
		got, ok := MaxGPUMemUtil(total, false, 0.01)
		if !ok {
			t.Fatalf("total=%d bytes reported no answer", total)
		}
		if got != 0 {
			t.Errorf("MaxGPUMemUtil(%d bytes) = %.4f, want 0", total, got)
		}
	}
}

// An unreported capacity is not a cap of 1: the caller has to be able to tell
// "this card leaves no room" from "nobody measured this card", because the
// second must leave the existing default alone.
func TestMaxGPUMemUtilReportsNoAnswerWithoutACapacity(t *testing.T) {
	for _, total := range []int64{0, -1} {
		if got, ok := MaxGPUMemUtil(total, false, 0.01); ok {
			t.Errorf("MaxGPUMemUtil(%d) = %.4f, ok=true; want no answer", total, got)
		}
	}
}

// Without a step the exact share comes back, so a caller whose control has no
// grid is not silently rounded onto one.
func TestMaxGPUMemUtilWithoutAStepIsExact(t *testing.T) {
	got, ok := MaxGPUMemUtil(12*gib, false, 0)
	if !ok || got <= 0.8333 || got >= 0.8334 {
		t.Errorf("MaxGPUMemUtil(12 GiB, step=0) = %.6f (ok=%v), want the exact 0.8333…", got, ok)
	}
}

// The reserve is what gpu_memory_utilization leaves behind; the shortfall is
// memory a CUDA process cannot get at all. The first contains the second, and
// a reading that added them would charge a card twice for the same bytes.
func TestTheDriverShortfallFitsInsideTheCardReserve(t *testing.T) {
	if shortfallBytes >= cardReserveBytes {
		t.Fatalf("shortfall %d is not inside the %d-byte reserve; one of the two comments is now wrong",
			shortfallBytes, cardReserveBytes)
	}
}

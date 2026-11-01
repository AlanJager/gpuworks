package store

import (
	"testing"
	"time"
)

// The awake and present time of some segments (a node page's figures):
// overlapping instances count once, a gap between them is not present, and
// only awake stretches are awake.
func TestSegmentCoverage(t *testing.T) {
	base := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	seg := func(state ResidencyState, from, to time.Duration) ResidencySegment {
		return ResidencySegment{State: state, From: base.Add(from), To: base.Add(to)}
	}
	awake, present := SegmentCoverage([]ResidencySegment{
		seg(ResidencyAwake, 0, time.Hour),
		seg(ResidencyAwake, 30*time.Minute, 90*time.Minute), // another instance, overlapping
		seg(ResidencyAsleep, 90*time.Minute, 2*time.Hour),
		seg(ResidencyStarting, 3*time.Hour, 3*time.Hour+10*time.Minute), // after a gap
		seg(ResidencySwitching, 3*time.Hour+5*time.Minute, 3*time.Hour+8*time.Minute),
		seg(ResidencyAwake, 4*time.Hour, 4*time.Hour), // empty
	})
	if awake != (90*time.Minute).Milliseconds() || present != (130*time.Minute).Milliseconds() {
		t.Fatalf("awake %v present %v, want 1h30m and 2h10m", time.Duration(awake)*time.Millisecond, time.Duration(present)*time.Millisecond)
	}
	if a, p := SegmentCoverage(nil); a != 0 || p != 0 {
		t.Fatalf("none = %d, %d", a, p)
	}
}

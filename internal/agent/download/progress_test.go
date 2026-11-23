package download

import (
	"testing"
	"time"
)

func TestRateMeterAveragesOverTheWindow(t *testing.T) {
	const mib = 1 << 20
	start := time.Unix(1_700_000_000, 0)
	var m rateMeter
	if got := m.add(start, 0); got != 0 {
		t.Fatalf("a single sample has no rate, got %d", got)
	}
	// One second in: the mean of what has been seen so far.
	if got := m.add(start.Add(time.Second), 10*mib); got != 10*mib {
		t.Fatalf("rate after 1s = %d, want %d", got, 10*mib)
	}
	// A one-second pause between bursts must not read as a stopped transfer.
	if got := m.add(start.Add(2*time.Second), 10*mib); got != 5*mib {
		t.Fatalf("rate across a 1s pause = %d, want %d", got, 5*mib)
	}
	if got := m.add(start.Add(4*time.Second), 40*mib); got != 10*mib {
		t.Fatalf("rate after the burst = %d, want %d", got, 10*mib)
	}
	// Samples older than the window stop counting: 40 MiB at 4s to 50 MiB at
	// 9s, with the 0s..2s samples dropped.
	if got := m.add(start.Add(9*time.Second), 50*mib); got != 2*mib {
		t.Fatalf("rate over the last window = %d, want %d", got, 2*mib)
	}
}

func TestRateMeterReadsZeroOnceTheTransferStops(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var m rateMeter
	m.add(start, 0)
	m.add(start.Add(time.Second), 1<<20)
	for s := 2; s <= 5; s++ {
		if got := m.add(start.Add(time.Duration(s)*time.Second), 1<<20); got == 0 {
			t.Fatalf("rate dropped to zero %ds into the window", s)
		}
	}
	// Every sample left in the window carries the same byte count.
	if got := m.add(start.Add(rateWindow+time.Second+time.Millisecond), 1<<20); got != 0 {
		t.Fatalf("stopped transfer still reports %d B/s", got)
	}
	// A counter that goes backwards (a restarted file) is not a negative rate.
	if got := m.add(start.Add(rateWindow+2*time.Second), 0); got != 0 {
		t.Fatalf("rate after the counter reset = %d, want 0", got)
	}
}

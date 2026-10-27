package connbudget

import (
	"context"
	"testing"
	"time"
)

const (
	kib        = 1 << 10
	slowWindow = 10 * time.Second
)

// crawlSession opens a session on a big pool with n leases held, judging with
// the production ratio and a 16 KiB/s floor.
func crawlSession(t *testing.T, n int) (*fakeClock, []*Lease, *Session) {
	t.Helper()
	clock := newClock()
	s := newTestSession(t, NewPool(64), clock, SessionConfig{Cap: 64, Fixed: n, SlowWindow: slowWindow, SlowFloor: 16 * kib})
	leases := make([]*Lease, n)
	for i := range leases {
		l, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Release)
		leases[i] = l
		l.Observe(1) // opens the lease's window
	}
	return clock, leases, s
}

// round plays one window: every connection delivers rates[i] bytes per second
// and reports it, in order. It returns which connections were judged crawling.
func round(clock *fakeClock, leases []*Lease, rates []int64) []bool {
	clock.Advance(slowWindow)
	crawling := make([]bool, len(leases))
	for i, l := range leases {
		crawling[i] = l.Observe(rates[i] * int64(slowWindow.Seconds()))
	}
	return crawling
}

func uniform(n int, rate int64) []int64 {
	rates := make([]int64, n)
	for i := range rates {
		rates[i] = rate
	}
	return rates
}

func count(flags []bool) (n int) {
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

func TestAConnectionUnderTheAbsoluteFloorIsRecycledAfterTwoWindows(t *testing.T) {
	clock, leases, _ := crawlSession(t, 1)
	if got := round(clock, leases, []int64{8 * kib}); got[0] {
		t.Fatal("one window under the floor recycled the connection: it can be a hiccup in a healthy transfer")
	}
	if got := round(clock, leases, []int64{8 * kib}); !got[0] {
		t.Fatal("a lone connection at 8 KiB/s for two windows was left alone")
	}
}

func TestAHiccupBelowTheFloorIsForgottenByAHealthyWindow(t *testing.T) {
	clock, leases, _ := crawlSession(t, 1)
	round(clock, leases, []int64{8 * kib})
	round(clock, leases, []int64{2 << 20})
	if got := round(clock, leases, []int64{8 * kib}); got[0] {
		t.Fatal("two slow windows with a healthy one between them were counted as in a row")
	}
}

func TestAWindowIsJudgedWhenItEndsNotBefore(t *testing.T) {
	clock, leases, _ := crawlSession(t, 1)
	clock.Advance(slowWindow / 2)
	if leases[0].Observe(1) {
		t.Fatal("a connection was judged before its window ended: a slow start must not count")
	}
}

func TestAWholeLinkThatIsSlowButAboveTheFloorRecyclesNothing(t *testing.T) {
	clock, leases, _ := crawlSession(t, 8)
	round(clock, leases, uniform(8, 40*kib)) // warm-up: everyone has a window behind them
	for i := 0; i < 3; i++ {
		if n := count(round(clock, leases, uniform(8, 40*kib))); n != 0 {
			t.Fatalf("round %d recycled %d of 8 connections that were all merely slow", i, n)
		}
	}
}

func TestANodeWhoseWholeLinkCrawlsNeverLosesAllConnectionsAtOnce(t *testing.T) {
	for _, n := range []int{2, 3, 8, 24} {
		clock, leases, _ := crawlSession(t, n)
		// Every connection is under the floor: the link, not a connection, is
		// the problem. Replacements may be tried, but never all at once.
		round(clock, leases, uniform(n, 8*kib)) // the first window: a strike each
		got := round(clock, leases, uniform(n, 8*kib))
		if recycled := count(got); recycled > n/2 {
			t.Fatalf("%d connections: %d recycled in one window, at most %d allowed", n, recycled, n/2)
		}
		if count(got) == 0 {
			t.Fatalf("%d connections all under the floor and none was recycled", n)
		}
	}
}

func TestTheQuotaRefreshesWithTheNextWindow(t *testing.T) {
	clock, leases, _ := crawlSession(t, 4)
	round(clock, leases, uniform(4, 8*kib)) // a strike each
	if got := count(round(clock, leases, uniform(4, 8*kib))); got != 2 {
		t.Fatalf("first window to recycle recycled %d, want 2 of 4", got)
	}
	if got := count(round(clock, leases, uniform(4, 8*kib))); got != 2 {
		t.Fatalf("second window recycled %d, want 2 of 4", got)
	}
}

func TestAConnectionFarBehindItsSiblingsIsRecycledEvenAboveTheFloor(t *testing.T) {
	clock, leases, _ := crawlSession(t, 6)
	rates := uniform(6, 2<<20)
	rates[3] = 100 * kib // above 16 KiB/s, but under a tenth of the 2 MiB/s median
	if got := round(clock, leases, rates); got[3] {
		t.Fatal("recycled on the first comparison: siblings' windows may be stale")
	}
	got := round(clock, leases, rates)
	if !got[3] || count(got) != 1 {
		t.Fatalf("recycled = %v, want only connection 3", got)
	}
}

func TestOneSlowSiblingAmongFastOnesIsNotMistakenForAFastLinksMedian(t *testing.T) {
	// The median, not the mean: one very fast connection must not make the
	// ordinary ones look slow.
	clock, leases, _ := crawlSession(t, 6)
	rates := uniform(6, 300*kib)
	rates[0] = 50 << 20
	round(clock, leases, rates)
	if n := count(round(clock, leases, rates)); n != 0 {
		t.Fatalf("%d ordinary connections recycled because one was very fast", n)
	}
}

func TestTheSiblingRuleNeedsAtLeastTwoSiblings(t *testing.T) {
	clock, leases, _ := crawlSession(t, 2)
	rates := []int64{2 << 20, 100 * kib}
	round(clock, leases, rates)
	if got := round(clock, leases, rates); got[1] {
		t.Fatal("with one sibling there is no median to be far below")
	}
}

func TestAConnectionThatCollapsedFromItsOwnPeakIsRecycledWhenTheOthersHoldUp(t *testing.T) {
	clock, leases, _ := crawlSession(t, 2)
	round(clock, leases, []int64{2 << 20, 2 << 20})
	round(clock, leases, []int64{2 << 20, 100 * kib})
	got := round(clock, leases, []int64{2 << 20, 100 * kib})
	if got[0] || !got[1] {
		t.Fatalf("recycled = %v, want only the one that collapsed", got)
	}
}

func TestAWholeLinkSlumpIsNotACollapseOfEachConnection(t *testing.T) {
	// Everyone fell to a twentieth of what they had, together: the link got
	// slow; replacing connections would not bring the rate back.
	clock, leases, _ := crawlSession(t, 4)
	round(clock, leases, uniform(4, 2<<20))
	for i := 0; i < 3; i++ {
		if n := count(round(clock, leases, uniform(4, 100*kib))); n != 0 {
			t.Fatalf("window %d of the slump: %d connections recycled for a slump that hit all of them", i, n)
		}
	}
}

func TestALoneConnectionThatCollapsedFromItsPeakIsRecycled(t *testing.T) {
	clock, leases, _ := crawlSession(t, 1)
	round(clock, leases, []int64{4 << 20})
	round(clock, leases, []int64{200 * kib})
	if got := round(clock, leases, []int64{200 * kib}); !got[0] {
		t.Fatal("a single connection that fell to a twentieth of its peak was kept")
	}
}

func TestAReleasedConnectionStopsBeingASibling(t *testing.T) {
	clock, leases, s := crawlSession(t, 3)
	round(clock, leases, []int64{2 << 20, 2 << 20, 100 * kib})
	leases[0].Release()
	leases[1].Release()
	// Only one connection is left; with no siblings it is not "far below"
	// anything and sits above the floor.
	clock.Advance(slowWindow)
	if leases[2].Observe(100 * kib * 10) {
		t.Fatal("a connection was judged against siblings that had already finished")
	}
	if s.InUse() != 1 {
		t.Fatalf("in use = %d", s.InUse())
	}
}

func TestObserveFeedsTheSessionsRateWithoutJudgingWhenDisabled(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(4), clock, SessionConfig{Cap: 4, Fixed: 2})
	l, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	clock.Advance(time.Hour)
	if l.Observe(1) {
		t.Fatal("judged with no SlowWindow configured")
	}
	if s.bytes.Load() != 1 {
		t.Fatalf("session bytes = %d", s.bytes.Load())
	}
}

func TestHostsGiveEachUpstreamItsOwnBudget(t *testing.T) {
	h := NewHosts(2)
	a, b := h.For("registry-a"), h.For("registry-b")
	if a == b || h.For("registry-a") != a {
		t.Fatal("pools are not one per key")
	}
	if a.Ceiling() != 2 {
		t.Fatalf("ceiling = %d", a.Ceiling())
	}
	h.SetCeiling(5)
	if a.Ceiling() != 5 || b.Ceiling() != 5 || h.For("registry-c").Ceiling() != 5 {
		t.Fatal("SetCeiling did not reach existing and new pools")
	}
	if NewHosts(0).Ceiling() != DefaultCeiling {
		t.Fatal("a ceiling below one is not the default")
	}
}

package pluginhost

import (
	"sync/atomic"
	"testing"
	"time"
)

// Default meter timestamps are elapsed nanoseconds, not Unix wall time.
// Wall-clock corrections must not consume the plugin's stall budget.
func TestDefaultStallClockUsesElapsedTime(t *testing.T) {
	started := time.Now()
	m := newStallMeter(nil)
	m.writeStarted()
	timestamp := m.writing.Load()
	elapsed := time.Since(started).Nanoseconds()
	if timestamp <= 0 || timestamp > elapsed+1 {
		t.Fatalf("fresh write timestamp = %d; want nonzero elapsed nanoseconds <= %d (not a wall timestamp)", timestamp, elapsed+1)
	}
	m.writeDone()
	if d, stalled := m.stalled(time.Second); d != 0 || stalled {
		t.Fatalf("finished write = %s, stalled=%v", d, stalled)
	}
}

// A body reader can be descheduled after reading its clock while another
// body is sent completely. Its later zero-to-pending transition must not
// replace the newer progress time with the timestamp it sampled before.
func TestPendingBodyCannotRollBackConcurrentProgress(t *testing.T) {
	origin := int64(1)
	sampled := make(chan struct{})
	resume := make(chan struct{})
	var calls atomic.Int32
	m := newStallMeter(func() int64 {
		if calls.Add(1) == 1 {
			close(sampled)
			<-resume
			return origin
		}
		return origin + time.Hour.Nanoseconds()
	})
	done := make(chan struct{})
	go func() { m.hold(1); close(done) }()
	<-sampled
	// Another call sends its body while the first reader is descheduled.
	m.hold(1)
	m.sent(1)
	if m.unsent.Load() != 0 {
		t.Fatal("the other body was not completely sent")
	}
	close(resume)
	<-done
	if d, stalled := m.stalled(time.Second); stalled || d != 0 {
		t.Fatalf("pending body's old clock replaced actual recent progress: age=%v stalled=%v", d, stalled)
	}
	if m.unsent.Load() != 1 {
		t.Fatalf("unsent=%d, want 1", m.unsent.Load())
	}
	m.dropped(1)
	if d, stalled := m.stalled(time.Second); stalled || d != 0 || m.unsent.Load() != 0 {
		t.Fatalf("canceled pending body: age=%v stalled=%v unsent=%d", d, stalled, m.unsent.Load())
	}
}

// A zero origin is offset by the default source, and the first nonzero
// elapsed tick must still be recognized as an active write or pending body.
func TestStallBudgetKeepsExactElapsedThreshold(t *testing.T) {
	for _, kind := range []string{"write", "pending body"} {
		t.Run(kind, func(t *testing.T) {
			clock := newFakeClock()
			clock.ns.Store(1)
			m := newStallMeter(clock.now)
			if kind == "write" {
				m.writeStarted()
			} else {
				m.hold(1)
			}
			clock.advance(time.Second)
			if d, stalled := m.stalled(time.Second); stalled || d != time.Second {
				t.Fatalf("at limit: age=%v stalled=%v", d, stalled)
			}
			clock.advance(time.Nanosecond)
			if d, stalled := m.stalled(time.Second); !stalled || d != time.Second+time.Nanosecond {
				t.Fatalf("after limit: age=%v stalled=%v", d, stalled)
			}
			if kind == "write" {
				m.writeDone()
			} else {
				m.dropped(1)
			}
			clock.advance(time.Hour)
			if d, stalled := m.stalled(time.Second); stalled || d != 0 {
				t.Fatalf("after completion/cancel: age=%v stalled=%v", d, stalled)
			}
		})
	}
}

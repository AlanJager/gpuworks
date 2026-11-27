package app

import (
	"testing"
	"time"
)

// A session that served for a while is followed by a nearly immediate
// reconnect, spread by jitter, instead of the one-second base delay: that
// second was added to every agent reconnect, and the request that hit it
// waited it out.
func TestReconnectDelayImmediateAfterLongSession(t *testing.T) {
	for i := 0; i < 50; i++ {
		b := newBackoff(time.Second, 30*time.Second)
		d := reconnectDelay(b, longSessionBeforeFastReconnect+time.Second)
		if d < 0 || d > fastReconnectJitter {
			t.Fatalf("delay after a long session = %v, want 0-%v of jitter", d, fastReconnectJitter)
		}
	}
}

// The short delay replaces the first tick of the sequence, not the whole
// sequence: a reconnect that then fails fast waits the usual 1s, 2s, 4s...
func TestReconnectDelayKeepsTheBackoffAfterTheFastReconnect(t *testing.T) {
	b := newBackoff(time.Second, 30*time.Second)
	if d := reconnectDelay(b, longSessionBeforeFastReconnect+time.Second); d > fastReconnectJitter {
		t.Fatalf("first delay = %v, want 0-%v", d, fastReconnectJitter)
	}
	if d := reconnectDelay(b, time.Second); d != time.Second {
		t.Fatalf("delay after the fast reconnect = %v, want the 1s base", d)
	}
	if d := reconnectDelay(b, time.Second); d != 2*time.Second {
		t.Fatalf("next delay = %v, want 2s", d)
	}
}

// Sessions that fail fast keep the existing jittered-free exponential
// sequence, unchanged.
func TestReconnectDelayFastSessionsKeepTheBackoffSequence(t *testing.T) {
	b := newBackoff(time.Second, 30*time.Second)
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if d := reconnectDelay(b, time.Second); d != w {
			t.Fatalf("delay %d = %v, want %v", i, d, w)
		}
	}
}

// Repeated fast failures still reach and stay at the 30s cap, whether or not
// a long session preceded them.
func TestBackoffRepeatedFastFailuresGrowToTheCap(t *testing.T) {
	b := newBackoff(time.Second, 30*time.Second)
	reconnectDelay(b, longSessionBeforeFastReconnect+time.Second)
	for i := 0; i < 12; i++ {
		reconnectDelay(b, time.Second)
	}
	if d := reconnectDelay(b, time.Second); d != 30*time.Second {
		t.Fatalf("delay at the cap = %v, want 30s", d)
	}
	if b.cur != 30*time.Second {
		t.Fatalf("backoff cursor = %v, want it capped at 30s", b.cur)
	}
}

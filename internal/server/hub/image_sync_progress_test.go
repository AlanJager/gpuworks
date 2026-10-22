package hub

import (
	"testing"
	"time"
)

func TestImageSyncProgressLimiterKeepsTimedAndTerminalUpdates(t *testing.T) {
	var limiter imageSyncProgressLimiter
	if !limiter.allow(0, false) {
		t.Fatal("first progress was suppressed")
	}
	if limiter.allow(100*time.Millisecond, false) {
		t.Fatal("small burst progress was not suppressed")
	}
	if limiter.allow(200*time.Millisecond, false) {
		t.Fatal("large byte delta bypassed the time throttle")
	}
	if !limiter.allow(1100*time.Millisecond, false) {
		t.Fatal("one-second progress threshold was suppressed")
	}
	if !limiter.allow(1250*time.Millisecond, true) {
		t.Fatal("forced terminal progress was suppressed")
	}
}

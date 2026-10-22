package download

import (
	"context"
	"sync"
	"time"
)

// rateLimiter is a shared token bucket limiting aggregate throughput. A nil
// *rateLimiter means unlimited. It starts empty so a fresh download is throttled
// from the first byte.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	tokens float64
	max    float64
	last   time.Time
}

// newRateLimiter returns a limiter for bytesPerSec, or nil when <= 0 (unlimited).
func newRateLimiter(bytesPerSec float64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	max := bytesPerSec
	if max < 4<<20 {
		max = 4 << 20
	}
	return &rateLimiter{rate: bytesPerSec, tokens: 0, max: max, last: time.Now()}
}

// Wait blocks until n tokens are available or ctx is done.
func (l *rateLimiter) Wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	need := float64(n)
	if need > l.max {
		need = l.max // avoid a permanent stall for oversized requests
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		l.last = now
		if l.tokens > l.max {
			l.tokens = l.max
		}
		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((need - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()

		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

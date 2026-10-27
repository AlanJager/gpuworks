package fakeregistry

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// parseRange reads "bytes=a-b", "bytes=a-" and "bytes=-n". partial is false
// when there is no Range header at all.
func parseRange(spec string, size int64) (start, end int64, partial, ok bool) {
	if spec == "" {
		return 0, size - 1, false, true
	}
	value, found := strings.CutPrefix(spec, "bytes=")
	if !found || strings.Contains(value, ",") {
		return 0, 0, false, false
	}
	first, last, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, false, false
	}
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		start, end = max(0, size-suffix), size-1
	} else {
		var err error
		if start, err = strconv.ParseInt(first, 10, 64); err != nil {
			return 0, 0, false, false
		}
		end = size - 1
		if last != "" {
			if end, err = strconv.ParseInt(last, 10, 64); err != nil {
				return 0, 0, false, false
			}
			end = min(end, size-1)
		}
	}
	if start < 0 || start >= size || end < start {
		return 0, 0, false, false
	}
	return start, end, true, true
}

// bucket is a rate limit that tells a writer how long to wait.
type bucket struct {
	mu   sync.Mutex
	rate float64
	free time.Time
}

func newBucket(rate int64) *bucket { return &bucket{rate: float64(rate)} }

// reserve books n bytes and returns how long to wait before sending them.
func (b *bucket) reserve(n int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.free.Before(now) {
		b.free = now
	}
	wait := b.free.Sub(now)
	b.free = b.free.Add(time.Duration(float64(n) / b.rate * float64(time.Second)))
	return wait
}

// Pattern is n deterministic bytes that differ between seeds, so a blob written
// at the wrong offset or from the wrong blob does not match.
func Pattern(seed, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + seed*131 + i>>8*7) % 251)
	}
	return b
}

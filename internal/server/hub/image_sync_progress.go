package hub

import (
	"sync"
	"time"
)

const (
	imageSyncProgressInterval = time.Second
)

// imageSyncProgressLimiter bounds progress writes to once per second. Callers
// force the final observation before a terminal update.
type imageSyncProgressLimiter struct {
	mu          sync.Mutex
	seen        bool
	lastElapsed time.Duration
}

func (l *imageSyncProgressLimiter) allow(elapsed time.Duration, force bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !force && l.seen && elapsed-l.lastElapsed < imageSyncProgressInterval {
		return false
	}
	l.seen = true
	l.lastElapsed = elapsed
	return true
}

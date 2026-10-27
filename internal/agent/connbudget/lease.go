package connbudget

import (
	"slices"
	"sync"
	"time"
)

// Lease is one request's token. A caller reading a response body reports each
// read to Observe, which tells it when this connection has become worth
// replacing.
type Lease struct {
	s    *Session
	once sync.Once

	// The window is only touched by the goroutine reading the connection.
	winStart time.Time
	winBytes int64

	// Guarded by the pool's mutex: what siblings read of this connection.
	judged   bool
	lastRate float64
	peak     float64
	strikes  int // consecutive windows judged slow by comparison
}

// Release gives the token back; it is safe to call more than once and on a
// lease of a nil session.
func (l *Lease) Release() {
	if l == nil || l.s == nil {
		return
	}
	l.once.Do(func() {
		p := l.s.pool
		p.mu.Lock()
		l.s.releaseLocked(l)
		p.mu.Unlock()
	})
}

// Observe counts n received bytes toward the session's rate and this
// connection's window, and reports whether the connection should be replaced
// for crawling. A window is judged when it ends, so a connection is never
// recycled for the slow start of a transfer that then picks up.
//
// A window that closes judges the connection three ways, each against the
// others in the same session so that a link that is slow as a whole does not
// look like a failing connection:
//
//   - below the absolute floor (SessionConfig.SlowFloor): a trickle on any link;
//   - below SlowRatio of the median of the sibling connections' last windows
//     (needs at least two siblings with a window behind them): one connection
//     far behind the rest;
//   - below SlowRatio of its own best window, while most siblings kept their
//     own: a connection that was fine and collapsed.
//
// A comparison must hold in two windows in a row before it recycles a
// connection (the siblings' windows end at other moments, so one window can
// compare against a link that has since changed); the floor acts at once.
// However many connections qualify, no more than half of the session's open
// connections are replaced within one window, so a pull is never torn down all
// at once; the rest are judged again at their next window.
func (l *Lease) Observe(n int64) (crawling bool) {
	if l == nil || l.s == nil || n <= 0 {
		return false
	}
	s := l.s
	s.bytes.Add(n)
	if s.cfg.SlowWindow <= 0 {
		return false
	}
	now := s.now()
	if l.winStart.IsZero() {
		l.winStart = now
	}
	l.winBytes += n
	elapsed := now.Sub(l.winStart)
	if elapsed < s.cfg.SlowWindow {
		return false
	}
	rate := float64(l.winBytes) / elapsed.Seconds()
	l.winStart, l.winBytes = now, 0
	return s.judge(l, now, rate)
}

func (s *Session) judge(l *Lease, now time.Time, rate float64) bool {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	l.lastRate, l.judged = rate, true
	l.peak = max(l.peak, rate)

	var siblings []float64
	healthy := 0
	for other := range s.leases {
		if other == l || !other.judged {
			continue
		}
		siblings = append(siblings, other.lastRate)
		if other.lastRate >= other.peak*0.5 {
			healthy++
		}
	}
	median := medianOf(siblings)
	var reason string
	relative := false
	switch {
	case rate < float64(s.cfg.SlowFloor):
		reason = "below the absolute floor"
	case len(siblings) >= 2 && rate < median*s.cfg.SlowRatio:
		reason, relative = "far below its siblings", true
	case rate < l.peak*s.cfg.SlowRatio && (len(siblings) == 0 || healthy*2 >= len(siblings)):
		reason, relative = "collapsed from its own peak", true
	default:
		l.strikes = 0
		return false
	}
	// A comparison is only as good as the siblings' last windows, which end at
	// other moments than this one: when the whole link slumps, the first
	// connection to close a window still sees siblings from before it. So a
	// connection that is slow by comparison must be so in two windows in a row,
	// by which time the siblings' windows have caught up. The absolute floor
	// needs no comparison and acts at once.
	if relative {
		if l.strikes++; l.strikes < 2 {
			return false
		}
	}
	// Never recycle the whole pull at once: at most half of the open
	// connections (one, when there is only one) per window.
	cutoff := now.Add(-s.cfg.SlowWindow)
	s.recycled = slices.DeleteFunc(s.recycled, func(t time.Time) bool { return !t.After(cutoff) })
	if len(s.recycled) >= max(1, len(s.leases)/2) {
		return false
	}
	s.recycled = append(s.recycled, now)
	s.logf("download connection recycled", "session", s.cfg.Name, "reason", reason, "rate_bytes_per_s", int64(rate), "sibling_median_bytes_per_s", int64(median), "peak_bytes_per_s", int64(l.peak), "open", len(s.leases))
	return true
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

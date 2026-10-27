package connbudget

import "time"

// The climb: how a session finds its connection count from its own throughput,
// and how it gives connections back when the server pushes back.

// Tick evaluates the session at now. The session's own goroutine calls it; it
// is exported so tests can drive a fake clock.
func (s *Session) Tick(now time.Time) {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	if s.closed {
		return
	}
	s.recoverLocked(now)
	dt := now.Sub(s.winStart)
	if dt < s.cfg.Window {
		return
	}
	bytes := s.bytes.Load()
	rate := float64(bytes-s.winBytes) / dt.Seconds()
	saturated := s.saturated
	s.winStart, s.winBytes, s.saturated, s.resets = now, bytes, false, 0
	if rate > 0 && now.Sub(s.lastBackoff) > 2*s.cfg.Window {
		s.throttles = 0 // the server answers again: the next refusal starts small
	}
	if s.fixed {
		return
	}
	if now.Before(s.holdUntil) {
		s.trial = false
		return
	}
	if !saturated {
		return // fewer requests wanted than the target: nothing to learn
	}
	s.evaluateLocked(now, rate)
}

func (s *Session) evaluateLocked(now time.Time, rate float64) {
	if !s.warmed {
		// The first window of a download measures connections still being set
		// up (TCP, TLS, redirects), which is no baseline: a doubling compared
		// against it always looks like a gain.
		s.warmed = true
		return
	}
	limit := s.limitLocked()
	if s.trial {
		s.trial = false
		if s.ref <= 0 || rate >= s.ref*(1+s.required) {
			s.logf("download connections kept", "session", s.cfg.Name, "connections", s.target, "rate_bytes_per_s", int64(rate), "previous_bytes_per_s", int64(s.ref))
			s.ref, s.refTarget = rate, s.target
			if !s.settled && s.target < limit {
				s.setTargetLocked(now, min(limit, s.target*2))
				s.trial, s.required = true, s.cfg.MinGain
				return
			}
			s.settleLocked(now)
			return
		}
		s.logf("download connections reverted: no gain", "session", s.cfg.Name, "tried", s.target, "back_to", s.refTarget, "rate_bytes_per_s", int64(rate), "previous_bytes_per_s", int64(s.ref))
		s.setTargetLocked(now, s.refTarget)
		s.settleLocked(now)
		return
	}
	s.ref, s.refTarget = rate, s.target
	if s.settled {
		if now.Before(s.nextProbe) {
			return
		}
		if s.target >= limit {
			s.nextProbe = now.Add(s.cfg.ProbeEvery)
			return
		}
		s.setTargetLocked(now, min(limit, max(s.target+1, s.target*5/4)))
		s.trial, s.required = true, probeGain
		return
	}
	if s.target >= limit {
		s.settleLocked(now)
		return
	}
	s.setTargetLocked(now, min(limit, s.target*2))
	s.trial, s.required = true, s.cfg.MinGain
}

func (s *Session) settleLocked(now time.Time) {
	s.settled = true
	s.nextProbe = now.Add(s.cfg.ProbeEvery)
}

func (s *Session) setTargetLocked(now time.Time, target int) {
	if target < 1 {
		target = 1
	}
	if target != s.target {
		s.logf("download connections changed", "session", s.cfg.Name, "from", s.target, "to", target)
	}
	s.target = target
	s.winStart, s.winBytes, s.saturated = now, s.bytes.Load(), false
	s.pool.dispatchLocked()
}

// recoverLocked lets a download that was throttled earn its ceiling back after
// a calm spell, one doubling at a time.
func (s *Session) recoverLocked(now time.Time) {
	if s.softCap == 0 || now.Before(s.holdUntil) {
		return
	}
	since := s.lastBackoff
	if s.lastRecover.After(since) {
		since = s.lastRecover
	}
	if now.Sub(since) < recoverAfter {
		return
	}
	s.lastRecover = now
	s.softCap *= 2
	if s.softCap >= s.cap {
		s.softCap = 0
	}
	s.settled, s.trial = false, false
	s.logf("download connections ceiling relaxed", "session", s.cfg.Name, "ceiling", s.limitLocked())
	if s.fixed && !s.noRange {
		s.setTargetLocked(now, min(s.cfg.Fixed, s.limitLocked()))
	}
}

// Throttled reports a 429/503 or a refused connection. The download halves its
// connections, stays below that level for a while, and every request of the
// pool waits retryAfter before it is made: a server that said "come back in N
// seconds" is not asked again sooner by any connection to it. An answer that
// names no time gets a short, doubling pause of its own, so "too many" is never
// answered by reconnecting at once.
func (s *Session) Throttled(retryAfter time.Duration) {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	now := s.now()
	if retryAfter <= 0 {
		retryAfter = min(throttleBase<<min(s.throttles, 4), throttleMax)
		s.throttles++
	}
	retryAfter = min(retryAfter, MaxPause)
	if until := now.Add(retryAfter); until.After(s.pauseUntil) {
		s.pauseUntil = until
	}
	if until := now.Add(retryAfter); until.After(s.pool.pauseUntil) {
		s.pool.pauseUntil = until
	}
	s.backoffLocked(now, "throttled")
}

// Reset reports a connection reset or a truncated body. One is routine on a
// long download; three within a window back the download off.
func (s *Session) Reset() {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	s.resets++
	if s.resets >= resetsPerBackoff {
		s.resets = 0
		s.backoffLocked(s.now(), "connection resets")
	}
}

func (s *Session) backoffLocked(now time.Time, why string) {
	if !s.lastBackoff.IsZero() && now.Sub(s.lastBackoff) < s.cfg.BackoffGuard {
		return // one burst of failures is one event
	}
	s.lastBackoff = now
	current := s.effectiveLocked()
	next := max(1, current/2)
	s.backoffs++
	hold := holdBase << min(s.backoffs-1, 4)
	if hold > holdMax {
		hold = holdMax
	}
	s.holdUntil = now.Add(hold)
	s.softCap = next
	s.trial, s.settled = false, true
	s.nextProbe = s.holdUntil
	s.logf("download connections backed off", "session", s.cfg.Name, "reason", why, "from", current, "to", next, "hold", hold.String())
	s.setTargetLocked(now, next)
}

// NoRange reports a server that ignores byte ranges: one connection, always.
func (s *Session) NoRange() {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	if s.noRange {
		return
	}
	s.cap, s.fixed, s.noRange = 1, true, true
	s.logf("download connections limited to one: the server ignores ranges", "session", s.cfg.Name)
	s.setTargetLocked(s.now(), 1)
}

func (s *Session) logf(msg string, kv ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(msg, kv...)
	}
}

// SetCap changes the most connections the session may use (the download moved
// to another address, which may take more or fewer).
func (s *Session) SetCap(limit int) {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	if limit < 1 {
		limit = 1
	}
	if s.noRange {
		return // a server that ignores ranges stays at one
	}
	s.cap = limit
	s.pool.dispatchLocked()
}

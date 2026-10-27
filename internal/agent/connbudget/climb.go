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
	// Demand that outran the target at any point of the window, or that is
	// still queued behind a full target as the window ends.
	saturated := s.saturated || (len(s.waiters) > 0 && s.inUse >= s.effectiveLocked())
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
	if s.trial && s.ramping {
		// The window right after the change measures connections that are still
		// being opened (TCP, TLS, redirects, slow start). Judging a doubling on
		// it makes every link with some latency look like "no gain"; the next
		// window, with the new connections at speed, is the one that counts.
		s.ramping = false
		return
	}
	if s.trial {
		s.trial = false
		if s.ref <= 0 || rate >= s.ref*(1+s.required) {
			s.retried = false
			s.logf("download connections kept", "session", s.cfg.Name, "connections", s.target, "rate_bytes_per_s", int64(rate), "previous_bytes_per_s", int64(s.ref))
			s.ref, s.refTarget = rate, s.target
			if !s.settled && s.target < limit {
				s.setTargetLocked(now, min(limit, s.target*2))
				s.trial, s.ramping, s.required = true, true, s.cfg.MinGain
				return
			}
			s.settleLocked(now)
			return
		}
		s.logf("download connections reverted: no gain", "session", s.cfg.Name, "tried", s.target, "back_to", s.refTarget, "rate_bytes_per_s", int64(rate), "previous_bytes_per_s", int64(s.ref))
		doubling := s.required == s.cfg.MinGain
		s.setTargetLocked(now, s.refTarget)
		if doubling && !s.retried {
			// One noisy window must not end the climb for minutes: the
			// doubling gets a second try before the level is settled.
			s.retried = true
			return
		}
		s.retried = false
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
		s.trial, s.ramping, s.required = true, true, probeGain
		return
	}
	if s.target >= limit {
		s.settleLocked(now)
		return
	}
	s.setTargetLocked(now, min(limit, s.target*2))
	s.trial, s.ramping, s.required = true, true, s.cfg.MinGain
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
		s.backoffs = 0 // fully recovered: the next backoff starts with the shortest hold
	}
	s.settled, s.trial, s.ramping, s.retried = false, false, false, false
	s.logf("download connections ceiling relaxed", "session", s.cfg.Name, "ceiling", s.limitLocked())
	if s.fixed {
		s.setTargetLocked(now, min(s.cfg.Fixed, s.limitLocked()))
	}
}

// Throttled reports a 429/503 or a refused connection. The download halves its
// connections, stays below that level for a while, and every request of the
// pool waits retryAfter before it is made: a server that said "come back in N
// seconds" is not asked again sooner by any connection to it. An answer that
// names no time gets a short, doubling pause of its own, so "too many" is never
// answered by reconnecting at once. The pause only escalates when the answer is
// a new event: a burst of simultaneous refusals is one event and shares the
// pause of the first.
func (s *Session) Throttled(retryAfter time.Duration) {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	now := s.now()
	fresh := s.freshEventLocked(now)
	if retryAfter <= 0 {
		streak := s.throttles
		if !fresh {
			streak = max(streak-1, 0) // the burst's own level, not the next one
		}
		retryAfter = min(throttleBase<<min(streak, 4), throttleMax)
	}
	if fresh {
		s.throttles++
	}
	retryAfter = min(retryAfter, MaxPause)
	if until := now.Add(retryAfter); until.After(s.pool.pauseUntil) {
		s.pool.pauseUntil = until
	}
	s.backoffLocked(now, "throttled", holdMax)
}

// Reset reports a connection reset or a truncated body. One is routine on a
// long download, and so are a few in a window when many connections are open:
// the download backs off only when resets in one window reach three or half of
// its open connections, and holds the lower level for at most a minute.
func (s *Session) Reset() {
	if s == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	s.resets++
	if s.resets >= max(resetsPerBackoff, s.inUse/2) {
		s.resets = 0
		s.backoffLocked(s.now(), "connection resets", resetHoldMax)
	}
}

// freshEventLocked reports whether a failure at now starts a new backoff event
// rather than belonging to the burst the last backoff answered.
func (s *Session) freshEventLocked(now time.Time) bool {
	return s.lastBackoff.IsZero() || now.Sub(s.lastBackoff) >= s.cfg.BackoffGuard
}

func (s *Session) backoffLocked(now time.Time, why string, maxHold time.Duration) {
	if !s.freshEventLocked(now) {
		return // one burst of failures is one event
	}
	s.lastBackoff = now
	current := s.effectiveLocked()
	next := max(1, current/2)
	s.backoffs++
	hold := min(holdBase<<min(s.backoffs-1, 4), maxHold)
	s.holdUntil = now.Add(hold)
	s.softCap = next
	s.trial, s.ramping, s.settled = false, false, true
	s.nextProbe = s.holdUntil
	s.logf("download connections backed off", "session", s.cfg.Name, "reason", why, "from", current, "to", next, "hold", hold.String())
	s.setTargetLocked(now, next)
}

func (s *Session) logf(msg string, kv ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(msg, kv...)
	}
}

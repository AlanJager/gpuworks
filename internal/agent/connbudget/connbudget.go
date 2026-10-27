// Package connbudget decides how many HTTP connections a node's downloads may
// have open at once against one upstream address.
//
// A Pool is the budget for one upstream (a registry host, a model hub, a
// proxy): no more than its ceiling of requests are in flight across every
// download that holds a Session on it, and a "come back later" answer from the
// upstream pauses all of them. A Session is one download (an image pull, a
// model repository). It asks for a Lease before every request and gives it
// back afterwards, and it carries its own target: how many of the pool's
// tokens this download may hold right now.
//
// The target is found by the download itself, from its own throughput. It
// starts low, doubles for as long as doubling still raises the aggregate rate
// by a meaningful share, and returns to the smaller level once it does not:
// a per-connection cap makes more connections pay, a per-address bandwidth cap
// or a slow disk does not, and the climb stops where the gain stops. A
// throttled answer (429/503), a refused connection or repeated resets halve
// the target and keep the download below the level that triggered them,
// down to a single connection, and a Retry-After is waited out by every
// connection before the next request. Nothing is remembered across downloads:
// every Session climbs from its own start.
//
// A Lease also judges its own connection against its siblings in the same
// session (see lease.go): one that has collapsed relative to the others is
// worth replacing, but a link that is slow as a whole is not a reason to
// replace every connection.
//
// The package is a leaf: it knows nothing about HTTP, files or tasks.
package connbudget

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultCeiling is the per-pool limit when none is configured.
	DefaultCeiling = 32
	// DefaultStart is how many connections a climbing download starts with.
	DefaultStart = 2

	defaultWindow     = 3 * time.Second
	defaultMinGain    = 0.15
	probeGain         = 0.05
	defaultProbeEvery = 60 * time.Second
	backoffGuard      = 2 * time.Second
	holdBase          = 30 * time.Second
	holdMax           = 8 * time.Minute
	recoverAfter      = 2 * time.Minute
	resetsPerBackoff  = 3
	// resetHoldMax bounds how long repeated resets keep a download from
	// climbing: a reset is weaker evidence of a limit than a 429, and a hold
	// that long would leave a large download at a fraction of its speed.
	resetHoldMax = time.Minute

	// MaxPause bounds how long one answer can make a download wait.
	MaxPause = 2 * time.Minute
	// throttleBase is the pause after a throttled answer that named no time; it
	// doubles for each consecutive such answer up to throttleMax, so a server
	// that says "too many" without saying when is not asked again at once.
	throttleBase = 2 * time.Second
	throttleMax  = 30 * time.Second

	defaultSlowRatio = 0.1
)

// ErrClosed is returned by Acquire once the session is closed.
var ErrClosed = errors.New("connection budget closed")

// Pool is the connection budget of one upstream address.
type Pool struct {
	mu         sync.Mutex
	ceiling    int
	inUse      int
	sessions   []*Session
	next       int
	pauseUntil time.Time
}

// NewPool returns a pool admitting at most ceiling requests at once; a
// ceiling below one means DefaultCeiling.
func NewPool(ceiling int) *Pool {
	if ceiling < 1 {
		ceiling = DefaultCeiling
	}
	return &Pool{ceiling: ceiling}
}

// SetCeiling changes the limit; requests already in flight finish.
func (p *Pool) SetCeiling(ceiling int) {
	if ceiling < 1 {
		ceiling = DefaultCeiling
	}
	p.mu.Lock()
	p.ceiling = ceiling
	p.dispatchLocked()
	p.mu.Unlock()
}

// Ceiling is the limit.
func (p *Pool) Ceiling() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ceiling
}

// InUse is how many requests hold a token right now.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inUse
}

// SessionConfig is one download's budget request.
type SessionConfig struct {
	// Name labels log lines.
	Name string
	// Cap is the most connections this download may use; zero means the pool's
	// ceiling.
	Cap int
	// Start is the first target of a climbing download; zero is DefaultStart.
	Start int
	// Fixed, when positive, is a target that never changes by climbing (a
	// rate-limited download gains nothing from more connections). Throttling
	// still lowers it.
	Fixed int
	// Window is how long the aggregate rate is measured before a decision;
	// zero is three seconds.
	Window time.Duration
	// MinGain is the rate increase doubling the target must bring to be kept;
	// zero is 15%.
	MinGain float64
	// ProbeEvery is how often a settled download tries a quarter more
	// connections; zero is a minute.
	ProbeEvery time.Duration
	// BackoffGuard is the shortest time between two backoffs: a burst of
	// failures from one overloaded moment is one event. Zero is two seconds.
	BackoffGuard time.Duration

	// SlowWindow, SlowFloor and SlowRatio say when a Lease reports its
	// connection as crawling (see Lease.Observe). A window of SlowWindow is
	// judged when it ends; the connection is slow when it averaged under
	// SlowFloor bytes per second, or under SlowRatio (a tenth if zero) of the
	// median of its siblings, or collapsed to under SlowRatio of its own peak
	// while its siblings did not. A zero SlowWindow disables the judgement.
	SlowWindow time.Duration
	SlowFloor  int64
	SlowRatio  float64

	// Now replaces the clock (tests).
	Now func() time.Time
	// Wait replaces the timer a request waits on while the upstream asked for
	// time (tests). It returns when d has passed or ctx is done.
	Wait func(ctx context.Context, d time.Duration) error
	// NoTicker leaves evaluation to explicit Tick calls (tests).
	NoTicker bool
	// Log receives decisions (raised, kept, backed off); optional.
	Log func(msg string, kv ...any)
}

type waiter struct {
	ch      chan struct{}
	lease   *Lease
	granted bool
}

// Session is one download's share of a Pool.
type Session struct {
	pool *Pool
	cfg  SessionConfig
	now  func() time.Time

	bytes atomic.Int64

	// Everything below is guarded by pool.mu.
	target    int
	cap       int
	softCap   int // 0: none; set by a backoff, relaxed after a calm spell
	inUse     int
	waiters   []*waiter
	leases    map[*Lease]struct{}
	recycled  []time.Time
	closed    bool
	fixed     bool // the target does not climb
	saturated bool
	warmed    bool // the first measured window has been set aside
	ramping   bool // the window after a trial's change is set aside: its new connections are still starting
	retried   bool // the failed doubling at this level has had its second try

	winStart  time.Time
	winBytes  int64
	ref       float64
	refTarget int
	trial     bool
	required  float64
	settled   bool
	nextProbe time.Time

	holdUntil   time.Time
	lastBackoff time.Time
	lastRecover time.Time
	backoffs    int
	throttles   int // consecutive throttled answers that named no time
	resets      int

	stop chan struct{}
}

// Open registers a download with the pool. Close it when the download ends.
func (p *Pool) Open(cfg SessionConfig) *Session {
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	if cfg.MinGain <= 0 {
		cfg.MinGain = defaultMinGain
	}
	if cfg.ProbeEvery <= 0 {
		cfg.ProbeEvery = defaultProbeEvery
	}
	if cfg.Start <= 0 {
		cfg.Start = DefaultStart
	}
	if cfg.BackoffGuard <= 0 {
		cfg.BackoffGuard = backoffGuard
	}
	if cfg.SlowRatio <= 0 {
		cfg.SlowRatio = defaultSlowRatio
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Session{pool: p, cfg: cfg, now: now, cap: cfg.Cap, stop: make(chan struct{}), leases: map[*Lease]struct{}{}}
	p.mu.Lock()
	if s.cap <= 0 {
		s.cap = math.MaxInt32 // the pool's ceiling is the only bound
	}
	s.target = cfg.Start
	if cfg.Fixed > 0 {
		s.target, s.fixed = cfg.Fixed, true
	}
	s.winStart = now()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	if !cfg.NoTicker {
		go s.run()
	}
	return s
}

func (s *Session) run() {
	interval := s.cfg.Window / 5
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Tick(s.now())
		}
	}
}

// Close releases the session. Requests still waiting fail with ErrClosed;
// leases already granted stay valid until released.
func (s *Session) Close() {
	if s == nil {
		return
	}
	p := s.pool
	p.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		for _, w := range s.waiters {
			close(w.ch)
		}
		s.waiters = nil
		for i, other := range p.sessions {
			if other == s {
				p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
				break
			}
		}
		if p.next >= len(p.sessions) {
			p.next = 0
		}
		p.dispatchLocked()
	}
	p.mu.Unlock()
}

// Bytes is how many bytes the session's connections have reported so far. A
// connection that fails can tell whether the others still made headway by
// comparing it before and after.
func (s *Session) Bytes() int64 {
	if s == nil {
		return 0
	}
	return s.bytes.Load()
}

// Target is the number of connections the session may hold now.
func (s *Session) Target() int {
	if s == nil {
		return 0
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return s.effectiveLocked()
}

// Ceiling is the pool's limit on requests in flight to the upstream, which a
// download may use as the number of workers it starts: the target and the
// pauses decide how many of them run, and a worker count frozen at a moment of
// backoff would stay low for a whole large download.
func (s *Session) Ceiling() int {
	if s == nil {
		return 1
	}
	return s.pool.Ceiling()
}

// InUse is how many tokens the session holds.
func (s *Session) InUse() int {
	if s == nil {
		return 0
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return s.inUse
}

// Acquire blocks until the session may open one more request and returns its
// Lease, which must be released when the request is over. While the upstream
// has asked for time (Throttled), Acquire waits it out first. A nil session
// admits everything.
func (s *Session) Acquire(ctx context.Context) (*Lease, error) {
	if s == nil {
		return &Lease{}, nil
	}
	var waited time.Time
	for {
		// Wait before queueing, so a pause is not spent holding a token that
		// another download of the same upstream could use.
		if until := s.pauseDeadline(); until.After(waited) {
			if d := until.Sub(s.now()); d > 0 {
				if err := s.wait(ctx, d); err != nil {
					return nil, err
				}
			}
			waited = until
		}
		lease, err := s.enqueue(ctx)
		if err != nil {
			return nil, err
		}
		// Another connection may have been told to wait while this one queued.
		if until := s.pauseDeadline(); until.After(waited) && s.now().Before(until) {
			lease.Release()
			continue
		}
		return lease, nil
	}
}

func (s *Session) enqueue(ctx context.Context) (*Lease, error) {
	p := s.pool
	p.mu.Lock()
	if s.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	w := &waiter{ch: make(chan struct{}), lease: &Lease{s: s}}
	s.waiters = append(s.waiters, w)
	p.dispatchLocked()
	p.mu.Unlock()

	select {
	case <-w.ch:
	case <-ctx.Done():
		p.mu.Lock()
		if w.granted {
			s.releaseLocked(w.lease)
		} else {
			s.removeWaiterLocked(w)
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	granted := w.granted
	p.mu.Unlock()
	if !granted {
		return nil, ErrClosed
	}
	return w.lease, nil
}

func (s *Session) wait(ctx context.Context, d time.Duration) error {
	if s.cfg.Wait != nil {
		return s.cfg.Wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pauseDeadline is when the upstream's last "come back later" runs out. The
// pause belongs to the pool: every download of the same address waits it out.
func (s *Session) pauseDeadline() time.Time {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return s.pool.pauseUntil
}

func (s *Session) removeWaiterLocked(w *waiter) {
	for i, other := range s.waiters {
		if other == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

func (s *Session) releaseLocked(l *Lease) {
	s.inUse--
	s.pool.inUse--
	delete(s.leases, l)
	s.pool.dispatchLocked()
}

// limitLocked is the highest target the session may have now.
func (s *Session) limitLocked() int {
	limit := s.cap
	if s.softCap > 0 && s.softCap < limit {
		limit = s.softCap
	}
	if s.pool.ceiling < limit {
		limit = s.pool.ceiling
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

func (s *Session) effectiveLocked() int {
	target := s.target
	if limit := s.limitLocked(); target > limit {
		target = limit
	}
	if target < 1 {
		target = 1
	}
	return target
}

// dispatchLocked hands out tokens: sessions take turns, so a second download
// is not starved behind the first one's queue of chunks.
func (p *Pool) dispatchLocked() {
	for p.inUse < p.ceiling && len(p.sessions) > 0 {
		granted := false
		n := len(p.sessions)
		for i := 0; i < n; i++ {
			idx := (p.next + i) % n
			s := p.sessions[idx]
			if len(s.waiters) == 0 || s.inUse >= s.effectiveLocked() {
				continue
			}
			w := s.waiters[0]
			s.waiters = s.waiters[1:]
			w.granted = true
			s.inUse++
			p.inUse++
			s.leases[w.lease] = struct{}{}
			close(w.ch)
			p.next = (idx + 1) % n
			granted = true
			break
		}
		if !granted {
			break
		}
	}
	// A session whose own target is full while more requests queue behind it
	// has more demand than it is serving: the only state in which a higher
	// target could be measured.
	for _, s := range p.sessions {
		if len(s.waiters) > 0 && s.inUse >= s.effectiveLocked() {
			s.saturated = true
		}
	}
}

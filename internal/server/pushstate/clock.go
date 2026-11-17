package pushstate

import (
	"sync"
	"time"
)

// extclock is the clock timelines follow: max(wall clock, high-water mark),
// where the high-water mark advances by monotonic elapsed time while the
// server runs and is persisted every minute and at shutdown (ADR 0071
// decision 9). Turning the wall clock back therefore cannot bring back a
// step that already fired; only an explicit correction moves it back, and a
// step is never consumed, so a correction un-fires the steps after it.
type extclock struct {
	wall func() time.Time
	mono func() time.Duration

	mu     sync.Mutex
	base   int64 // the high-water mark at anchor, Unix ms
	anchor time.Duration
}

// MaxClockCorrection bounds how far one correction may move extclock.
const MaxClockCorrection = 366 * 24 * time.Hour

// Clock is where extclock reads time; the zero value reads the system
// clock. Tests inject both readings.
type Clock struct {
	// Wall is the wall clock.
	Wall func() time.Time
	// Mono is a monotonic reading: elapsed time since any fixed origin.
	Mono func() time.Duration
}

var processStart = time.Now()

func newExtclock(c Clock, hwmMS int64) *extclock {
	e := &extclock{wall: c.Wall, mono: c.Mono}
	if e.wall == nil {
		e.wall = time.Now
	}
	if e.mono == nil {
		e.mono = func() time.Duration { return time.Since(processStart) }
	}
	e.start(hwmMS)
	return e
}

// start anchors extclock at max(wall clock, hwmMS), the persisted high-water
// mark, from which it advances by monotonic time.
func (e *extclock) start(hwmMS int64) {
	w := e.wall().UnixMilli()
	e.mu.Lock()
	e.base, e.anchor = max(w, hwmMS), e.mono()
	e.mu.Unlock()
}

// nowMS is extclock now, in Unix milliseconds.
func (e *extclock) nowMS() int64 {
	w := e.wall().UnixMilli()
	e.mu.Lock()
	m := e.base + (e.mono() - e.anchor).Milliseconds()
	e.mu.Unlock()
	return max(w, m)
}

// correct sets extclock's high-water mark to to and returns the extclock it
// replaced. The wall clock still bounds it from below: a correction undoes a
// high-water mark carried forward by a clock that ran ahead, once the wall
// clock is right again.
func (e *extclock) correct(toMS int64) (fromMS int64) {
	from := e.nowMS()
	e.mu.Lock()
	e.base, e.anchor = toMS, e.mono()
	e.mu.Unlock()
	return from
}

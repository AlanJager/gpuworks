package imagepull

import "time"

// crawlWatch tells a pull that is making headway, but too slowly to be worth
// waiting for, from one that is merely slow to start. It averages the bytes
// received over whole windows, so a burst or a pause inside a window does not
// matter, only what the window as a whole delivered.
type crawlWatch struct {
	window  time.Duration
	minRate int64

	started time.Time
	base    int64
	running bool
}

// observe is called at every beat with the bytes received so far. judged is
// false whenever the pull cannot be told to crawl (not downloading, or a
// runtime whose counter moves in coarse steps); that also restarts the window,
// so time spent unpacking or waiting never counts against a source. crawling is
// true, with the window's average rate, when a window ended under minRate.
func (w *crawlWatch) observe(now time.Time, done int64, judged bool) (rate int64, crawling bool) {
	if !judged || w.window <= 0 || w.minRate <= 0 {
		w.running = false
		return 0, false
	}
	if !w.running || done < w.base {
		// First beat, or a layer restarted and the total fell back.
		w.started, w.base, w.running = now, done, true
		return 0, false
	}
	elapsed := now.Sub(w.started)
	if elapsed < w.window {
		return 0, false
	}
	rate = int64(float64(done-w.base) / elapsed.Seconds())
	w.started, w.base = now, done
	return rate, rate < w.minRate
}

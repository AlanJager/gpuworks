package download

import (
	"context"
	"time"
)

// reporter periodically invokes an onProgress callback (~4x/sec) with an
// aggregate Done count and a throughput computed over a short sliding window.
type reporter struct {
	stopCh chan struct{}
	doneCh chan struct{}
}

func startReporter(ctx context.Context, total int64, getDone func() int64, cb func(Progress)) *reporter {
	r := &reporter{stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	if cb == nil {
		close(r.doneCh)
		return r
	}
	go func() {
		defer close(r.doneCh)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()

		type sample struct {
			t time.Time
			d int64
		}
		var window []sample

		emit := func() {
			now := time.Now()
			d := getDone()
			window = append(window, sample{now, d})
			for len(window) > 1 && now.Sub(window[0].t) > time.Second {
				window = window[1:]
			}
			var rate int64
			if len(window) >= 2 {
				first := window[0]
				last := window[len(window)-1]
				if dt := last.t.Sub(first.t).Seconds(); dt > 0 {
					rate = int64(float64(last.d-first.d) / dt)
				}
			}
			cb(Progress{Total: total, Done: d, RateBytesPerS: rate})
		}

		for {
			select {
			case <-r.stopCh:
				emit() // final sample
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				emit()
			}
		}
	}()
	return r
}

func (r *reporter) stop() {
	close(r.stopCh)
	<-r.doneCh
}

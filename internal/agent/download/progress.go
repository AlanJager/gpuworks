package download

import (
	"context"
	"time"
)

// rateWindow is how far back the reported throughput looks. A remaining-time
// estimate divides by this rate, so it has to ride out the bursts of a
// multi-connection or throttled transfer; it is still short enough that a
// transfer that stops reads as zero within seconds.
const rateWindow = 5 * time.Second

type rateSample struct {
	t time.Time
	d int64
}

// rateMeter is the mean throughput over the last rateWindow of samples.
type rateMeter struct{ window []rateSample }

func (m *rateMeter) add(now time.Time, done int64) int64 {
	m.window = append(m.window, rateSample{now, done})
	for len(m.window) > 1 && now.Sub(m.window[0].t) > rateWindow {
		m.window = m.window[1:]
	}
	first, last := m.window[0], m.window[len(m.window)-1]
	if dt := last.t.Sub(first.t).Seconds(); dt > 0 && last.d > first.d {
		return int64(float64(last.d-first.d) / dt)
	}
	return 0
}

// reporter periodically invokes an onProgress callback (~4x/sec) with an
// aggregate Done count and a throughput computed over rateWindow.
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

		var meter rateMeter
		emit := func() {
			d := getDone()
			cb(Progress{Total: total, Done: d, RateBytesPerS: meter.add(time.Now(), d)})
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

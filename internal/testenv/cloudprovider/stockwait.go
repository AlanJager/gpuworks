package cloudprovider

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

const (
	// DefaultStockWait is how long a stock-limited provider keeps polling for
	// capacity before the test gives up. Sold-out specs usually come back
	// within minutes to an hour; the nightly peak (15:00 Asia/Shanghai) is the
	// case this budget is sized for.
	DefaultStockWait = 30 * time.Minute
	// DefaultStockPollInterval is the pause between two availability probes.
	DefaultStockPollInterval = 5 * time.Minute
)

// StockWait configures wait-and-retry for a provider whose capacity runs out
// (every candidate refuses a create for lack of stock). Max <= 0 disables
// waiting: the first stock-out is returned unchanged.
//
// A retry only ever follows a stock-out, which by definition means the
// provider created nothing, so no instance is billed while waiting. Any other
// error ends the loop at once.
type StockWait struct {
	Max      time.Duration // total wait budget measured from the first attempt
	Interval time.Duration // pause between attempts; DefaultStockPollInterval when zero

	// Test seams; nil means the real clock, a context-aware sleep and log.Printf.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Logf  func(format string, args ...any)
}

// StockWaiter is implemented by providers that wait out a stock-out inside
// Rent, so callers can size their own deadlines around the wait.
type StockWaiter interface {
	StockWait() time.Duration
}

// StockWaitFromEnv builds a StockWait from a duration environment variable
// ("45m", "90s"). Empty uses def; "0" disables waiting.
func StockWaitFromEnv(name string, def time.Duration) (StockWait, error) {
	wait := StockWait{Max: def, Interval: DefaultStockPollInterval}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return wait, nil
	}
	limit, err := time.ParseDuration(raw)
	if err != nil || limit < 0 {
		return StockWait{}, fmt.Errorf("invalid %s %q: want a non-negative duration such as 30m", name, raw)
	}
	wait.Max = limit
	return wait, nil
}

// RetryOnStockOut runs attempt until it succeeds, fails with an error that
// isStockOut rejects, ctx ends, or the wait budget is spent. Only a stock-out
// is retried. On budget exhaustion the last stock-out error is returned,
// wrapped with the wait summary so errors.Is/As still see the original.
func RetryOnStockOut[T any](ctx context.Context, w StockWait, label string, isStockOut func(error) bool, attempt func(context.Context) (T, error)) (T, error) {
	now, sleep, logf := w.Now, w.Sleep, w.Logf
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepContext
	}
	if logf == nil {
		logf = log.Printf
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultStockPollInterval
	}
	start := now()
	for round := 1; ; round++ {
		value, err := attempt(ctx)
		if err == nil || !isStockOut(err) || w.Max <= 0 {
			return value, err
		}
		remaining := w.Max - now().Sub(start)
		if remaining <= 0 {
			return value, fmt.Errorf("%w (%s still out of stock after waiting %s over %d rounds)", err, label, w.Max, round)
		}
		pause := min(interval, remaining)
		logf("%s: out of stock (round %d), waiting %s before retrying (%s of %s budget left): %v", label, round, pause, remaining, w.Max, err)
		if sleepErr := sleep(ctx, pause); sleepErr != nil {
			return value, fmt.Errorf("%w (%s wait for stock interrupted after %d rounds: %w)", err, label, round, sleepErr)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"
	"time"
)

// Run owns this synchronous loop through its business wait group. In particular,
// cancellation waits for an in-progress durable publication before Store.Close.
func runTransportAuthorization(ctx context.Context, refresh func(context.Context) error, log *slog.Logger, interval, timeout time.Duration) {
	failing := false
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		err := refresh(attempt)
		if err == nil {
			err = attempt.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil && !failing {
			// Deliberately omit low-level errors containing certificate or policy data.
			log.Warn("transport authorization refresh failed; retaining current live authorization")
		} else if err == nil && failing {
			log.Info("transport authorization refresh recovered")
		}
		failing = err != nil
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

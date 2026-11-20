package api

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// settingsWriteRetryBudget bounds the whole settings write, every attempt
// included, below the edge helper's per-origin attempt limit.
const settingsWriteRetryBudget = 4 * time.Second

// setSettingWithWriteRetry repeats one settings upsert while it fails with a
// transient cluster error. The budget is measured from the first attempt and
// also bounds each attempt, so a slow attempt cannot extend the request. A
// leadership change can interrupt the commit with an unknown outcome;
// repeating it is safe only because the same key and value are written again:
// the row converges, the per-setting notification only gains a revision, and
// the network hook sees no change. Other writes must not reuse this. A closed
// write admission is definitive even when joined with a driver error. The
// caller's context cancels the wait immediately, and the last attempt's error
// is returned when the budget is exhausted.
func setSettingWithWriteRetry(ctx context.Context, write func(context.Context) error, budget time.Duration) error {
	// time.Now keeps its monotonic reading through UTC, so wall-clock steps
	// cannot stretch or cut the budget.
	deadline := time.Now().UTC().Add(budget)
	attemptCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	backoff := 25 * time.Millisecond
	var lastTransient error
	for {
		err := write(attemptCtx)
		if err == nil || errors.Is(err, store.ErrBusinessWritesClosed) || !transientClusterReadError(err) {
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			// A retry that started just inside the budget and was cut by it
			// says nothing new: the cluster error that caused the retry is
			// still the answer, so the edge may hand the write to another member.
			if err != nil && lastTransient != nil && attemptCtx.Err() != nil {
				return lastTransient
			}
			return err
		}
		lastTransient = err
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		timer := time.NewTimer(min(backoff, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if time.Until(deadline) <= 0 {
			return err
		}
		backoff = min(backoff*2, 400*time.Millisecond)
	}
}

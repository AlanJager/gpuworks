//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errCandidateMaterialNoMajority means no peer completed a vote and none
// reported a changed manifest. It is not evidence that canonical bytes moved.
var errCandidateMaterialNoMajority = errors.New("candidate material copies do not form a majority")

// runCandidateHedgedPeerQuorum avoids an unnecessary grant-bearing read when
// one complete peer is enough, while retaining a second slot for a slow peer.
// Waiting consumes the caller's existing budget and never extends authority.
func runCandidateHedgedPeerQuorum(ctx context.Context, count, required, parallel int, delay time.Duration, check func(context.Context, int) (bool, error)) error {
	if check == nil {
		return runCandidatePeerQuorum(ctx, count, required, parallel, nil)
	}
	return runCandidateProgressHedgedPeerQuorum(ctx, count, required, parallel, delay, func(ctx context.Context, index int, started, _ func()) (bool, error) {
		started()
		return check(ctx, index)
	})
}

// The primary starts its hedge clock only after acquiring its remote slot.
// Complete verified batches renew that clock; headers and partial bytes do not.
// These signals schedule the backup only and never contribute a quorum vote.
func runCandidateProgressHedgedPeerQuorum(ctx context.Context, count, required, parallel int, delay time.Duration, check func(context.Context, int, func(), func()) (bool, error)) error {
	if check == nil {
		return runCandidatePeerQuorum(ctx, count, required, parallel, nil)
	}
	if required != 1 || count < 2 || parallel < 2 {
		return runCandidatePeerQuorum(ctx, count, required, parallel, func(ctx context.Context, index int) (bool, error) {
			return check(ctx, index, func() {}, func() {})
		})
	}
	if deadline, ok := ctx.Deadline(); ok {
		delay = min(delay, time.Until(deadline)/4)
	}
	boundedDelay := func() time.Duration {
		if deadline, ok := ctx.Deadline(); ok {
			return min(delay, time.Until(deadline)/4)
		}
		return delay
	}
	backupReady := make(chan struct{})
	var release sync.Once
	allowBackup := func() { release.Do(func() { close(backupReady) }) }
	var mu sync.Mutex
	var timer *time.Timer
	var expires time.Time
	active := false
	var onIdle func()
	onIdle = func() {
		mu.Lock()
		defer mu.Unlock()
		if !active || ctx.Err() != nil {
			return
		}
		// A completed batch can renew while an old timer callback is queued.
		// That stale callback must observe the new deadline before releasing.
		if remaining := time.Until(expires); remaining > 0 {
			timer.Reset(remaining)
			return
		}
		allowBackup()
	}
	started := func() {
		mu.Lock()
		defer mu.Unlock()
		if active || ctx.Err() != nil {
			return
		}
		active = true
		wait := boundedDelay()
		expires = time.Now().Add(wait)
		timer = time.AfterFunc(wait, onIdle)
	}
	progress := func() {
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-backupReady:
			return
		default:
		}
		if active && ctx.Err() == nil {
			wait := boundedDelay()
			expires = time.Now().Add(wait)
			timer.Reset(wait)
		}
	}
	defer func() {
		mu.Lock()
		active = false
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
	}()
	return runCandidatePeerQuorum(ctx, count, required, parallel, func(workCtx context.Context, index int) (bool, error) {
		if index != 0 {
			select {
			case <-workCtx.Done():
				return false, workCtx.Err()
			case <-backupReady:
			}
			if err := workCtx.Err(); err != nil {
				return false, err
			}
		}
		began, verified := func() {}, func() {}
		if index == 0 {
			began, verified = started, progress
		}
		complete, err := check(workCtx, index, began, verified)
		if index == 0 && !complete && err == nil {
			allowBackup()
		}
		return complete, err
	})
}

// runCandidatePeerQuorum counts only complete independently verified peers.
// The caller already proved its local copy and supplies the remaining required
// copies and the process's existing worker bound. Reaching the required copies
// cancels unfinished peers without counting them. Every worker is joined before
// the caller can perform its final source grants and complete pointer recheck.
func runCandidatePeerQuorum(ctx context.Context, count, required, parallel int, check func(context.Context, int) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count < 1 || required < 1 || required > count || parallel < 1 || check == nil {
		return errors.New("candidate peer quorum requires bounded complete copies")
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var mu sync.Mutex
	verified := 0
	quorumReached := false
	var firstErr error
	for range min(parallel, count) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					complete, err := check(workCtx, index)
					mu.Lock()
					if err != nil {
						// Only this proof's successful majority may cancel a losing
						// worker. Actual integrity errors remain fatal even if they
						// return concurrently with that cancellation.
						winnerCanceled := quorumReached && workCtx.Err() == context.Canceled && errors.Is(err, context.Canceled)
						if !winnerCanceled && firstErr == nil {
							firstErr = err
							cancel()
						}
					} else if complete && workCtx.Err() == nil {
						verified++
						if verified >= required {
							quorumReached = true
							cancel()
						}
					}
					mu.Unlock()
					if err != nil || workCtx.Err() != nil {
						return
					}
				}
			}
		}()
	}
submit:
	for index := range count {
		select {
		case <-workCtx.Done():
			break submit
		case jobs <- index:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	if verified < required {
		return errCandidateMaterialNoMajority
	}
	return nil
}

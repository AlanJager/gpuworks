//go:build linux && dqlite

package app

import (
	"context"
	"sync"
)

// A candidate reads at most two remote source/peer streams at once, even when
// callers start independent proofs. Each stream performs its batch grants in
// sequence, so this also bounds grant pressure initiated by this process.
const candidateRemoteConcurrency = 2

var candidateRemoteSlots = make(chan struct{}, candidateRemoteConcurrency)

func withCandidateRemoteSlot(ctx context.Context, check func(context.Context) error) error {
	select {
	case candidateRemoteSlots <- struct{}{}:
		defer func() { <-candidateRemoteSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return check(ctx)
}

// runBoundedCandidateChecks generates indices without storing a quadratic
// source/peer task list. On the first failure it cancels and joins both workers
// before the caller can proceed to its final membership/pointer checks.
func runBoundedCandidateChecks(ctx context.Context, count int, check func(context.Context, int) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var first sync.Once
	var firstErr error
	for range min(candidateRemoteConcurrency, count) {
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
					if err := check(workCtx, index); err != nil {
						first.Do(func() { firstErr = err; cancel() })
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
	return firstErr
}

//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// At most two attempts run, each owning sequential physical connections.
// Success still joins the losing worker and rechecks the parent after cleanup.
func discoverSparseRaftContacts(ctx context.Context, seeds []string, read func(context.Context, string) (map[string]string, error)) (map[string]string, error) {
	if len(seeds) == 0 || len(seeds) > 512 || read == nil {
		return nil, errors.New("sparse discovery requires bounded Raft seeds")
	}
	seen := make(map[string]bool, len(seeds))
	for _, seed := range seeds {
		canonical, err := identity.NormalizeMemberAddress(seed)
		if err != nil || canonical != seed || seen[seed] {
			return nil, errors.New("sparse Raft seeds must be canonical and unique")
		}
		seen[seed] = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type attemptResult struct {
		contacts map[string]string
		err      error
	}
	workersCtx, cancel := context.WithCancel(ctx)
	jobs := make(chan string)
	results := make(chan attemptResult, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for seed := range jobs {
				if workersCtx.Err() != nil {
					return
				}
				contacts, err := read(workersCtx, seed)
				results <- attemptResult{contacts, err}
			}
		}()
	}
	var once sync.Once
	join := func() { once.Do(func() { cancel(); close(jobs); workers.Wait() }) }
	defer join()
	next, active := 0, 0
	failures := []error{errors.New("no authenticated Raft seed supplied public contacts")}
	for next < len(seeds) || active != 0 {
		var dispatch chan string
		var seed string
		if next < len(seeds) && active < 2 {
			dispatch, seed = jobs, seeds[next]
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case dispatch <- seed:
			next++
			active++
		case attempt := <-results:
			active--
			if attempt.err == nil && len(attempt.contacts) != 0 {
				join()
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return attempt.contacts, nil
			}
			failures = append(failures, attempt.err)
		}
	}
	return nil, errors.Join(failures...)
}

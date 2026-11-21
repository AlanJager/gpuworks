package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// transientClusterReadError reports whether a replicated read failed because
// the local cluster node is mid-leadership-change, so a bounded retry can
// succeed on the next leader. Anything else (constraint violations, invalid
// stored values, caller cancellation) is returned to the caller unchanged.
// backend is the store's: only it knows its driver's no-leader error.
func transientClusterReadError(backend store.RuntimeBackend, err error) bool {
	switch {
	case err == nil:
		return false
	// The node's gateway closes in-flight reads with a bare EOF when the local
	// server drops its proxy during demotion or shutdown (go-dqlite app proxy).
	case errors.Is(err, io.EOF):
		return true
	// database/sql reports an exhausted bad-connection retry as
	// driver.ErrBadConn; the go-dqlite driver maps its not-leader and
	// leadership-lost protocol codes (ErrIoErrNotLeader,
	// ErrIoErrLeadershipLost and their legacy codes) to ErrBadConn.
	case errors.Is(err, driver.ErrBadConn):
		return true
	default:
		// Dial-time failure while no leader is elected yet.
		return backend.ReplicatedLeaderUnavailable(err)
	}
}

// retryableSettingsRead adds, for this side-effect-free read only, a dqlite
// failure frame that interrupted the result during a leadership transfer.
// Writes keep transientClusterReadError alone.
func retryableSettingsRead(backend store.RuntimeBackend, err error) bool {
	return transientClusterReadError(backend, err) || backend.ReplicatedReadInterrupted(err)
}

// allSettingsWithReadRetry repeats one replicated read while it fails with a
// transient cluster error, inside a bounded budget measured from the first
// failure. The caller's context cancels the wait immediately. The last error
// is returned when the budget is exhausted; it is never replaced by a
// synthetic timeout.
func allSettingsWithReadRetry(ctx context.Context, backend store.RuntimeBackend, read func() (map[string]string, error), budget time.Duration) (map[string]string, error) {
	settings, err := read()
	if err == nil || !retryableSettingsRead(backend, err) {
		return settings, err
	}
	deadline := time.Now().UTC().Add(budget)
	backoff := 25 * time.Millisecond
	for {
		wait := backoff
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		settings, err = read()
		if err == nil || !retryableSettingsRead(backend, err) {
			return settings, err
		}
		backoff = min(backoff*2, 400*time.Millisecond)
	}
}

// lostDatabaseConnection reports a driver failure caused by leadership loss or
// a dropped replicated-database connection.
func lostDatabaseConnection(backend store.RuntimeBackend, err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		backend.ReplicatedLeaderUnavailable(err)
}

// settingsStoreErrorStatus is the status for a settings read or write the
// store failed after its bounded retry. A connection still lost then means
// this member cannot reach a leader: 503 lets the edge, which retries only
// 502 and 503, send the request to another member. The write is safe to send
// again because the same key and value converge. Anything else stays 500.
func settingsStoreErrorStatus(backend store.RuntimeBackend, err error) int {
	if lostDatabaseConnection(backend, err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

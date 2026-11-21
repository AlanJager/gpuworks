//go:build linux && dqlite

package api

import (
	"fmt"
	"net/http"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// During an election the replicated driver cannot open a connection and
// reports its no-leader sentinel. The settings read retries on it, and when
// the budget ends the request is handed to another member.
func TestNoAvailableLeaderIsRetriedAndThenHandedOff(t *testing.T) {
	for _, err := range []error{
		dqlitedriver.ErrNoAvailableLeader,
		fmt.Errorf("open connection: %w", dqlitedriver.ErrNoAvailableLeader),
	} {
		if !transientClusterReadError(store.ReplicatedBackend(), err) || !retryableSettingsRead(store.ReplicatedBackend(), err) {
			t.Errorf("%v is not retried", err)
		}
		if got := settingsStoreErrorStatus(store.ReplicatedBackend(), err); got != http.StatusServiceUnavailable {
			t.Errorf("%v status=%d", err, got)
		}
		// The SQLite backend does not know the replicated driver's sentinel.
		if transientClusterReadError(store.RuntimeBackend{}, err) || settingsStoreErrorStatus(store.RuntimeBackend{}, err) != http.StatusInternalServerError {
			t.Errorf("%v classified without the replicated backend", err)
		}
	}
}

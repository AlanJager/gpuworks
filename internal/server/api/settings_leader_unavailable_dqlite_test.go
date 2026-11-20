//go:build linux && dqlite

package api

import (
	"fmt"
	"net/http"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// During an election the replicated driver cannot open a connection and
// reports its no-leader sentinel. The settings read retries on it, and when
// the budget ends the request is handed to another member.
func TestNoAvailableLeaderIsRetriedAndThenHandedOff(t *testing.T) {
	for _, err := range []error{
		dqlitedriver.ErrNoAvailableLeader,
		fmt.Errorf("open connection: %w", dqlitedriver.ErrNoAvailableLeader),
	} {
		if !transientClusterReadError(err) || !retryableSettingsRead(err) {
			t.Errorf("%v is not retried", err)
		}
		if got := settingsStoreErrorStatus(err); got != http.StatusServiceUnavailable {
			t.Errorf("%v status=%d", err, got)
		}
	}
}

package api

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Only a lost database connection turns a failed settings read or write into
// 503; every other store failure stays a server error on this member.
func TestSettingsStoreErrorStatusSeparatesLostConnectionFromStoreFailure(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		driver.ErrBadConn,
		sql.ErrConnDone,
		fmt.Errorf("read settings: %w", driver.ErrBadConn),
		errors.Join(driver.ErrBadConn, errors.New("leader stepped down")),
	} {
		if got := settingsStoreErrorStatus(store.RuntimeBackend{}, err); got != http.StatusServiceUnavailable {
			t.Errorf("%v status=%d", err, got)
		}
	}
	for _, err := range []error{
		errors.New("constraint failed"),
		errors.New("invalid stored value"),
		store.ErrMaintenanceNotServing,
		// A message that only reads like a driver failure is not one.
		errors.New("EOF"),
		errors.New("driver: bad connection"),
	} {
		if got := settingsStoreErrorStatus(store.RuntimeBackend{}, err); got != http.StatusInternalServerError {
			t.Errorf("%v status=%d", err, got)
		}
	}
	if lostDatabaseConnection(store.RuntimeBackend{}, nil) {
		t.Error("nil error reported as a lost connection")
	}
}

//go:build linux && dqlite

package app

import dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

func materialCheckpointContention(err error) bool {
	for err != nil {
		if _, multiple := err.(interface{ Unwrap() []error }); multiple {
			return false
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			err = wrapped.Unwrap()
			continue
		}
		// The pinned native gateway rejects Exec/Query before execution when
		// its checkpoint lock is held: SQLITE_BUSY, exactly this message.
		native, ok := err.(dqlitedriver.Error)
		return ok && native.Code == 5 && native.Message == "checkpoint in progress"
	}
	return false
}

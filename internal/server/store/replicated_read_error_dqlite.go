//go:build linux && dqlite

package store

import (
	"errors"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// ReplicatedReadInterrupted reports a dqlite server failure response that
// interrupted a read. During a leadership transfer a multi-part result can
// end in a failure frame whose code and text are leftover stream bytes (for
// example code 3 "key", or "EOF"); the next read on a fresh request
// succeeds. Callers may retry only side-effect-free reads on it.
// go-dqlite returns driver.Error as a value, which errors.As matches here.
func ReplicatedReadInterrupted(err error) bool {
	var failure dqlitedriver.Error
	return errors.As(err, &failure)
}

// ReplicatedLeaderUnavailable reports a dial that found no elected leader.
// go-dqlite returns this sentinel as the root cause when a connection cannot
// be opened during an election.
func ReplicatedLeaderUnavailable(err error) bool {
	return errors.Is(err, dqlitedriver.ErrNoAvailableLeader)
}

// ReplicatedWriterBusy reports a dqlite failure whose primary code is
// SQLITE_BUSY: the statement did not run because another connection held the
// database's one writer for the node's whole busy timeout (or a checkpoint
// was in progress). It says nothing about the data the statement would have
// read or written.
func ReplicatedWriterBusy(err error) bool {
	var failure dqlitedriver.Error
	return errors.As(err, &failure) && failure.Code&0xff == dqlitedriver.ErrBusy
}

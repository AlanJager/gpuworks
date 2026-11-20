// Package filelock provides process-scoped advisory locks for local state.
package filelock

import "errors"

var (
	ErrBusy        = errors.New("file lock is held by another process")
	ErrUnsupported = errors.New("file locking is supported on Linux and macOS only")
)

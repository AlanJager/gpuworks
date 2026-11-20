//go:build !linux || !dqlite

package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// openServerDatabase opens the database a test prepares before New in the
// form this build's server owns: a SQLite file here.
func openServerDatabase(t *testing.T, path string) (*store.Store, error) {
	t.Helper()
	return store.Open(path, bus.New())
}

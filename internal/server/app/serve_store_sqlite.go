//go:build !linux || !dqlite

package app

import (
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// openClosedMember never finds a member: this build has no replicated
// database, so every server is a single node.
func openClosedMember(Config, *bus.Bus, *slog.Logger) (*App, error) { return nil, nil }

// openServeStore opens the single-node SQLite store.
func openServeStore(cfg Config, events *bus.Bus, log *slog.Logger) (*store.Store, string, haRuntime, error) {
	st, master, err := openCredentialStore(cfg, events, log)
	return st, master, nil, err
}

//go:build !linux || !dqlite

package app

// memberState is empty without the replicated database: every server is a
// single node.
type memberState struct{}

//go:build !linux || !dqlite

package app

import "context"

// askMember is Host.AskMember on a server without the cluster runtime.
func (a *App) askMember(context.Context, uint64, string, []byte) ([]byte, error) {
	return nil, ErrNotClusterMember
}

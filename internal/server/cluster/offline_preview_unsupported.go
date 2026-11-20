//go:build !linux || !dqlite

package cluster

import "context"

// ExportOfflineSingleDatabase requires the native Linux dqlite implementation.
func ExportOfflineSingleDatabase(context.Context, string, string, Member, string) error {
	return ErrUnavailable
}

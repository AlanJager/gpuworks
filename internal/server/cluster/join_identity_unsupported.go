//go:build !linux || !dqlite

package cluster

import "context"

func PrepareJoinIdentity(context.Context, string, string, []string) (uint64, error) {
	return 0, ErrUnavailable
}

func ReconcileCommittedJoinMarker(context.Context, string, []Member) error {
	return ErrUnavailable
}

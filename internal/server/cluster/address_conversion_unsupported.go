//go:build !linux || !dqlite

package cluster

import "context"

// PrepareSingleMemberAddress requires the native Linux dqlite implementation.
func PrepareSingleMemberAddress(context.Context, string, string, DatabaseConfig) (Member, error) {
	return Member{}, ErrUnavailable
}

// PrepareSingleMemberAddressUnderLease requires the native Linux implementation.
func PrepareSingleMemberAddressUnderLease(context.Context, string, string, DatabaseConfig, []byte) (Member, error) {
	return Member{}, ErrUnavailable
}

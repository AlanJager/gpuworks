//go:build !linux || !dqlite

package cluster

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ProbeMember requires the native protocol implementation and performs no I/O
// on unsupported builds, including when supplied a malformed endpoint.
func ProbeMember(context.Context, string, string, *identity.Trust, []byte, []byte, identity.TransportPolicy) error {
	return ErrUnavailable
}

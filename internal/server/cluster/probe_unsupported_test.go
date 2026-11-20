//go:build !linux || !dqlite

package cluster

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestProbeMemberUnsupported(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, candidate := range []context.Context{t.Context(), ctx} {
		if err := ProbeMember(candidate, "not an address", "not a key", nil, nil, nil, identity.TransportPolicy{}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unsupported probe did not reject without inspection: %v", err)
		}
	}
}

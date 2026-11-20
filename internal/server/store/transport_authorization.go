package store

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// TransportAuthorizationEnabled reports a configured native transport owner.
// SQL fixtures and plaintext runtimes have no policy to update.
func (s *Store) TransportAuthorizationEnabled() bool { return s.refreshTransportAuthorization != nil }

// RefreshTransportAuthorization obtains a committed authorization snapshot for
// the bound local member. Its transport owner separately enforces additive-only
// live changes; removal and revocation require an explicit offline operation.
func (s *Store) RefreshTransportAuthorization(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.refreshTransportAuthorization == nil {
		return nil
	}
	return s.refreshTransportAuthorization(ctx)
}

type transportAuthorizationConfirmation func(context.Context, string, string) (identity.TransportPolicy, error)

// ConfirmTransportAuthorization acknowledges a currently applied runtime policy
// only after a fresh majority-confirmed grant to the authenticated requester.
// expectedLocalKey binds the request's HTTPS server identity to the local native
// transport member. This method never applies a pending policy as a side effect.
func (s *Store) ConfirmTransportAuthorization(ctx context.Context, requesterKey, expectedLocalKey string) (identity.TransportPolicy, error) {
	if err := ctx.Err(); err != nil {
		return identity.TransportPolicy{}, err
	}
	if s.confirmTransportAuthorization == nil {
		return identity.TransportPolicy{}, errors.New("runtime transport authorization confirmation is unavailable")
	}
	return s.confirmTransportAuthorization(ctx, requesterKey, expectedLocalKey)
}

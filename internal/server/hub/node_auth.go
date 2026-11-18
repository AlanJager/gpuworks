package hub

import (
	"crypto/hmac"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// authNodeToken authenticates the current per-node token. The token itself is
// the identity; a stale/unknown UUID cannot make a valid token fail.
func (h *Hub) authNodeToken(token string) (*store.Node, error) {
	n, err := h.store.NodeByTokenHash(store.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, errBadToken
	}
	if err != nil {
		return nil, fmt.Errorf("look up node token: %w", err)
	}
	if !hmac.Equal([]byte(n.TokenHash), []byte(store.HashToken(token))) {
		return nil, errBadToken
	}
	return n, nil
}

package enrollment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// FetchClusterQuorumFacts reads one bounded observation from a known provider.
// The caller must establish the provider origin and key independently. This
// observation does not authorize a member start or prove all voters closed.
func (c *Client) FetchClusterQuorumFacts(ctx context.Context, certificate, key []byte, expectedProviderKey string) (ClusterQuorumFacts, error) {
	if !validCatalogDigest(expectedProviderKey) {
		return ClusterQuorumFacts{}, errors.New("quorum facts require a canonical expected provider key")
	}
	return c.fetchClusterQuorumFacts(ctx, certificate, key, expectedProviderKey)
}

// FetchObservedQuorumFacts reads one leader snapshot without a caller-supplied
// provider key. The snapshot is kept only when the TLS peer is the voter
// leader it names. It does not authorize a member start or a role change.
func (c *Client) FetchObservedQuorumFacts(ctx context.Context, certificate, key []byte) (ClusterQuorumFacts, error) {
	return c.fetchClusterQuorumFacts(ctx, certificate, key, "")
}

func (c *Client) fetchClusterQuorumFacts(ctx context.Context, certificate, key []byte, expectedProviderKey string) (ClusterQuorumFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reject := func(err error) (ClusterQuorumFacts, error) {
		return ClusterQuorumFacts{}, errors.Join(err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return reject(err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	body, err := json.Marshal(ClusterQuorumFactsRequest{Nonce: nonce})
	if err != nil {
		return reject(err)
	}
	data, peerKey, err := c.memberRequestForPeerWithBodyAndKey(ctx, certificate, key, ClusterQuorumFactsPath, body, 512<<10, expectedProviderKey, http.StatusOK)
	if err != nil {
		return reject(err)
	}
	result, err := DecodeClusterQuorumFacts(data)
	if err != nil {
		return reject(err)
	}
	if result.ClusterID != c.clusterID || result.CAFingerprint != c.pin || result.Protocol != proto.ClusterProtocolVersion || result.Nonce != nonce {
		return reject(errors.New("quorum facts identity or nonce rejected"))
	}
	if expectedProviderKey != "" && peerKey != expectedProviderKey {
		return reject(errors.New("quorum facts provider identity rejected"))
	}
	for _, member := range result.Members {
		if member.ID == result.LeaderID && member.Role == "voter" && member.KeyFingerprint == peerKey {
			if err := ctx.Err(); err != nil {
				return reject(err)
			}
			return result, nil
		}
	}
	return reject(errors.New("quorum facts provider is not the observed voter leader"))
}

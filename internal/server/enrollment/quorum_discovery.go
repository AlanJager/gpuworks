package enrollment

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

const ClusterQuorumDiscoveryPath = "/_cluster/quorum-discovery"

// ClusterQuorumDiscovery contains contact hints, not quorum evidence, grants,
// process closure or permission to start a member.
type ClusterQuorumDiscovery struct {
	ClusterID     string          `json:"cluster_id"`
	CAFingerprint string          `json:"ca_fingerprint"`
	Protocol      uint            `json:"protocol"`
	Nonce         string          `json:"nonce"`
	Members       []ClusterMember `json:"members"`
}

func DecodeClusterQuorumDiscovery(data []byte) (ClusterQuorumDiscovery, error) {
	reject := func() (ClusterQuorumDiscovery, error) {
		return ClusterQuorumDiscovery{}, errors.New("invalid cluster quorum discovery hints")
	}
	if len(data) > 512<<10 {
		return reject()
	}
	fields, err := membershipObject(data, "cluster_id", "ca_fingerprint", "protocol", "nonce", "members")
	if err != nil {
		return reject()
	}
	var result ClusterQuorumDiscovery
	if err := json.Unmarshal(data, &result); err != nil || !validCatalogDigest(result.Nonce) {
		return reject()
	}
	clusterID, err := hex.DecodeString(result.ClusterID)
	if err != nil || len(clusterID) != 16 || strings.ToLower(result.ClusterID) != result.ClusterID || !validCatalogDigest(result.CAFingerprint) || result.Protocol != proto.ClusterProtocolVersion {
		return reject()
	}
	// Keep the original member JSON for exact-key and canonical tuple validation.
	// This synthetic revision is solely input to the existing decoder, never a
	// persisted or returned authorization revision.
	membership, err := json.Marshal(map[string]json.RawMessage{
		"cluster_id": fields["cluster_id"], "ca_fingerprint": fields["ca_fingerprint"], "protocol": fields["protocol"],
		"source_revision": json.RawMessage(`"1"`), "members": fields["members"],
	})
	if err != nil {
		return reject()
	}
	snapshot, err := decodeClusterMembership(membership)
	if err != nil {
		return reject()
	}
	result.Members = snapshot.Members
	return result, nil
}

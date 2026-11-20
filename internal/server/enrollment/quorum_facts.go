package enrollment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

const ClusterQuorumFactsPath = "/_cluster/quorum-facts"

type ClusterQuorumFactsRequest struct {
	Nonce string `json:"nonce"`
}

// ClusterQuorumFacts observes one leader snapshot bracketed by native barriers.
// It is not all-voter process closure or authorization to start a spare member.
// Its native configuration/index facts differ from membership grant revisions.
type ClusterQuorumFacts struct {
	ClusterID          string          `json:"cluster_id"`
	CAFingerprint      string          `json:"ca_fingerprint"`
	Protocol           uint            `json:"protocol"`
	Nonce              string          `json:"nonce"`
	Legacy             bool            `json:"legacy"`
	Phase              string          `json:"phase"`
	PhaseRevision      uint64          `json:"phase_revision,string"`
	Term               uint64          `json:"term,string"`
	Index              uint64          `json:"index,string"`
	ConfigurationIndex uint64          `json:"configuration_index,string"`
	LeaderID           uint64          `json:"leader_id,string"`
	Members            []ClusterMember `json:"members"`
}

func DecodeClusterQuorumFactsRequest(data []byte) (ClusterQuorumFactsRequest, error) {
	reject := func() (ClusterQuorumFactsRequest, error) {
		return ClusterQuorumFactsRequest{}, errors.New("invalid cluster quorum facts request")
	}
	if len(data) > 4<<10 {
		return reject()
	}
	if _, err := membershipObject(data, "nonce"); err != nil {
		return reject()
	}
	var request ClusterQuorumFactsRequest
	if err := json.Unmarshal(data, &request); err != nil || !validCatalogDigest(request.Nonce) {
		return reject()
	}
	return request, nil
}

func DecodeClusterQuorumFacts(data []byte) (ClusterQuorumFacts, error) {
	reject := func() (ClusterQuorumFacts, error) {
		return ClusterQuorumFacts{}, errors.New("invalid cluster quorum facts observation")
	}
	if len(data) > 512<<10 {
		return reject()
	}
	fields, err := membershipObject(data, "cluster_id", "ca_fingerprint", "protocol", "nonce", "legacy", "phase", "phase_revision", "term", "index", "configuration_index", "leader_id", "members")
	if err != nil {
		return reject()
	}
	var result ClusterQuorumFacts
	if err := json.Unmarshal(data, &result); err != nil {
		return reject()
	}
	clusterID, err := hex.DecodeString(result.ClusterID)
	if err != nil || len(clusterID) != 16 || strings.ToLower(result.ClusterID) != result.ClusterID || !validCatalogDigest(result.CAFingerprint) || result.Protocol != proto.ClusterProtocolVersion || !validCatalogDigest(result.Nonce) {
		return reject()
	}
	legacy := bytes.TrimSpace(fields["legacy"])
	if !bytes.Equal(legacy, []byte("true")) && !bytes.Equal(legacy, []byte("false")) {
		return reject()
	}
	phase := bytes.TrimSpace(fields["phase"])
	if len(phase) == 0 || phase[0] != '"' {
		return reject()
	}
	if result.Legacy {
		var revision string
		if result.Phase != "" || result.PhaseRevision != 0 || json.Unmarshal(fields["phase_revision"], &revision) != nil || revision != "0" {
			return reject()
		}
	} else {
		switch result.Phase {
		case "serving", "freezing", "frozen", "drained", "upgrading", "recovering":
		default:
			return reject()
		}
		if !membershipDecimal(fields["phase_revision"], result.PhaseRevision) {
			return reject()
		}
	}
	if !membershipDecimal(fields["term"], result.Term) || !membershipDecimal(fields["index"], result.Index) ||
		!membershipDecimal(fields["configuration_index"], result.ConfigurationIndex) || !membershipDecimal(fields["leader_id"], result.LeaderID) {
		return reject()
	}
	if result.ConfigurationIndex > result.Index {
		return reject()
	}
	// Preserve the original member JSON so duplicate/case/unknown fields cannot
	// disappear through a permissive struct decode before existing validation.
	membership, err := json.Marshal(map[string]json.RawMessage{
		"cluster_id": fields["cluster_id"], "ca_fingerprint": fields["ca_fingerprint"], "protocol": fields["protocol"],
		"source_revision": json.RawMessage(strconv.Quote(strconv.FormatUint(result.Index, 10))), "members": fields["members"],
	})
	if err != nil {
		return reject()
	}
	snapshot, err := decodeClusterMembership(membership)
	if err != nil {
		return reject()
	}
	result.Members = snapshot.Members
	for _, member := range result.Members {
		if member.ID == result.LeaderID && member.Role == "voter" {
			return result, nil
		}
	}
	return reject()
}

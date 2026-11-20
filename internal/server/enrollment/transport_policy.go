package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

const TransportPolicyPath = "/_cluster/transport-policy"

// TransportPolicySnapshot is the source's authorization snapshot, not a local
// policy publication. SourceRevision must never be used as a local CAS revision.
type TransportPolicySnapshot struct {
	ClusterID      string   `json:"cluster_id"`
	CAFingerprint  string   `json:"ca_fingerprint"`
	Protocol       uint     `json:"protocol"`
	SourceRevision uint64   `json:"source_revision,string"`
	AllowedKeys    []string `json:"allowed_keys"`
	RevokedKeys    []string `json:"revoked_keys"`
}

// FetchTransportPolicy authenticates both members before reading one bounded
// snapshot. The caller must separately validate and durably publish any policy.
func (c *Client) FetchTransportPolicy(ctx context.Context, certificate, key []byte) (TransportPolicySnapshot, error) {
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return TransportPolicySnapshot{}, err
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		return TransportPolicySnapshot{}, err
	}
	data, err := c.memberRequest(ctx, certificate, key, TransportPolicyPath, 128<<10)
	if err != nil {
		return TransportPolicySnapshot{}, err
	}
	result, err := decodeTransportPolicySnapshot(data)
	if err != nil {
		return TransportPolicySnapshot{}, err
	}
	if result.ClusterID != c.clusterID || result.CAFingerprint != c.pin || result.Protocol != proto.ClusterProtocolVersion || result.SourceRevision == 0 {
		return TransportPolicySnapshot{}, errors.New("transport policy identity or revision rejected")
	}
	if result.AllowedKeys == nil || result.RevokedKeys == nil || len(result.AllowedKeys) == 0 || len(result.AllowedKeys)+len(result.RevokedKeys) > 512 {
		return TransportPolicySnapshot{}, errors.New("transport policy requires bounded nonnull key arrays")
	}
	allowed := make(map[string]bool, len(result.AllowedKeys))
	for _, keys := range [][]string{result.AllowedKeys, result.RevokedKeys} {
		for i, key := range keys {
			if !validCatalogDigest(key) || (i > 0 && keys[i-1] >= key) {
				return TransportPolicySnapshot{}, errors.New("transport policy keys must be sorted unique lowercase SHA256 fingerprints")
			}
		}
	}
	for _, key := range result.AllowedKeys {
		allowed[key] = true
	}
	for _, key := range result.RevokedKeys {
		if allowed[key] {
			return TransportPolicySnapshot{}, errors.New("transport policy authorizes a revoked key")
		}
	}
	if !allowed[fingerprint] {
		return TransportPolicySnapshot{}, errors.New("transport policy does not authorize requesting member")
	}
	if err := ctx.Err(); err != nil {
		return TransportPolicySnapshot{}, err
	}
	return result, nil
}

func decodeTransportPolicySnapshot(data []byte) (TransportPolicySnapshot, error) {
	reject := func() (TransportPolicySnapshot, error) {
		return TransportPolicySnapshot{}, errors.New("invalid transport policy snapshot JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return reject()
	}
	fields := map[string]json.RawMessage{"cluster_id": nil, "ca_fingerprint": nil, "protocol": nil, "source_revision": nil, "allowed_keys": nil, "revoked_keys": nil}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return reject()
		}
		name, ok := token.(string)
		if !ok {
			return reject()
		}
		previous, known := fields[name]
		if !known || previous != nil {
			return reject()
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return reject()
		}
		fields[name] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return reject()
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return reject()
	}
	for _, value := range fields {
		if value == nil {
			return reject()
		}
	}
	var result TransportPolicySnapshot
	if err := json.Unmarshal(data, &result); err != nil {
		return reject()
	}
	var revision string
	if err := json.Unmarshal(fields["source_revision"], &revision); err != nil || revision != strconv.FormatUint(result.SourceRevision, 10) {
		return reject()
	}
	return result, nil
}

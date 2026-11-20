package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"gpuworks.ai/gpuworks/internal/proto"
)

const TransportAuthorizationPath = "/_cluster/transport-authorization"

// TransportAuthorizationReceipt confirms one named member's runtime policy.
// Its local revision is independent from the source authorization revision.
type TransportAuthorizationReceipt struct {
	ClusterID      string `json:"cluster_id"`
	CAFingerprint  string `json:"ca_fingerprint"`
	Protocol       uint   `json:"protocol"`
	MemberKey      string `json:"member_key"`
	SourceRevision uint64 `json:"source_revision,string"`
	LocalRevision  uint64 `json:"local_revision,string"`
}

// FetchTransportAuthorization authenticates the actual TLS peer as the expected
// policy owner before sending HTTP. A response's member_key is not identity proof.
func (c *Client) FetchTransportAuthorization(ctx context.Context, certificate, key []byte, expectedMemberKey string) (TransportAuthorizationReceipt, error) {
	if !validCatalogDigest(expectedMemberKey) {
		return TransportAuthorizationReceipt{}, errors.New("invalid expected member key")
	}
	data, err := c.memberRequestForPeer(ctx, certificate, key, TransportAuthorizationPath, 8<<10, expectedMemberKey)
	if err != nil {
		return TransportAuthorizationReceipt{}, err
	}
	result, err := decodeTransportAuthorization(data)
	if err != nil {
		return TransportAuthorizationReceipt{}, err
	}
	if result.ClusterID != c.clusterID || result.CAFingerprint != c.pin || result.Protocol != proto.ClusterProtocolVersion || result.MemberKey != expectedMemberKey || result.SourceRevision == 0 || result.LocalRevision == 0 {
		return TransportAuthorizationReceipt{}, errors.New("transport authorization receipt identity or revision rejected")
	}
	if err := ctx.Err(); err != nil {
		return TransportAuthorizationReceipt{}, err
	}
	return result, nil
}

func decodeTransportAuthorization(data []byte) (TransportAuthorizationReceipt, error) {
	reject := func() (TransportAuthorizationReceipt, error) {
		return TransportAuthorizationReceipt{}, errors.New("invalid transport authorization receipt JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return reject()
	}
	fields := map[string]json.RawMessage{"cluster_id": nil, "ca_fingerprint": nil, "protocol": nil, "member_key": nil, "source_revision": nil, "local_revision": nil}
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
	var result TransportAuthorizationReceipt
	if err := json.Unmarshal(data, &result); err != nil {
		return reject()
	}
	for name, value := range map[string]uint64{"source_revision": result.SourceRevision, "local_revision": result.LocalRevision} {
		var text string
		if err := json.Unmarshal(fields[name], &text); err != nil || text != strconv.FormatUint(value, 10) {
			return reject()
		}
	}
	return result, nil
}

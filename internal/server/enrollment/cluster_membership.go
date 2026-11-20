package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

const ClusterMembershipPath = "/_cluster/members"
const ClusterEnrollmentOriginPath = "/_cluster/enrollment-origin"

type ClusterMember struct {
	ID               uint64 `json:"id,string"`
	Address          string `json:"address"`
	Role             string `json:"role"`
	KeyFingerprint   string `json:"key_fingerprint"`
	EnrollmentOrigin string `json:"enrollment_origin,omitempty"`
}

// ClusterMembershipSnapshot is discovery information, not proof of readiness
// or permission to mutate Raft membership. Requesters may not yet be members.
type ClusterMembershipSnapshot struct {
	ClusterID      string          `json:"cluster_id"`
	CAFingerprint  string          `json:"ca_fingerprint"`
	Protocol       uint            `json:"protocol"`
	SourceRevision uint64          `json:"source_revision,string"`
	Members        []ClusterMember `json:"members"`
}

func (c *Client) FetchClusterMembership(ctx context.Context, certificate, key []byte) (ClusterMembershipSnapshot, error) {
	data, err := c.memberRequest(ctx, certificate, key, ClusterMembershipPath, 128<<10)
	if err != nil {
		return ClusterMembershipSnapshot{}, err
	}
	return c.validateClusterMembership(ctx, data)
}

// FetchClusterMembershipWithPeerKey binds the returned directory to the TLS
// member that served this request. Callers can pin a later write to that key.
func (c *Client) FetchClusterMembershipWithPeerKey(ctx context.Context, certificate, key []byte) (ClusterMembershipSnapshot, string, error) {
	data, peerKey, err := c.memberRequestForPeerWithBodyAndKey(ctx, certificate, key, ClusterMembershipPath, nil, 128<<10, "", 200)
	if err != nil {
		return ClusterMembershipSnapshot{}, "", err
	}
	if peerKey == "" {
		return ClusterMembershipSnapshot{}, "", errors.New("membership response omitted its authenticated peer")
	}
	result, err := c.validateClusterMembership(ctx, data)
	if err != nil {
		return ClusterMembershipSnapshot{}, "", err
	}
	for _, member := range result.Members {
		if member.KeyFingerprint == peerKey {
			return result, peerKey, nil
		}
	}
	return ClusterMembershipSnapshot{}, "", errors.New("serving member is absent from authenticated membership")
}

func (c *Client) validateClusterMembership(ctx context.Context, data []byte) (ClusterMembershipSnapshot, error) {
	result, err := decodeClusterMembership(data)
	if err != nil {
		return ClusterMembershipSnapshot{}, err
	}
	if result.ClusterID != c.clusterID || result.CAFingerprint != c.pin || result.Protocol != proto.ClusterProtocolVersion {
		return ClusterMembershipSnapshot{}, errors.New("cluster membership identity rejected")
	}
	if err := ctx.Err(); err != nil {
		return ClusterMembershipSnapshot{}, err
	}
	return result, nil
}

func decodeClusterMembership(data []byte) (ClusterMembershipSnapshot, error) {
	reject := func() (ClusterMembershipSnapshot, error) {
		return ClusterMembershipSnapshot{}, errors.New("invalid cluster membership snapshot")
	}
	fields, err := membershipObject(data, "cluster_id", "ca_fingerprint", "protocol", "source_revision", "members")
	if err != nil {
		return reject()
	}
	var result ClusterMembershipSnapshot
	if err := json.Unmarshal(data, &result); err != nil {
		return reject()
	}
	if !membershipDecimal(fields["source_revision"], result.SourceRevision) || result.Members == nil || len(result.Members) == 0 || len(result.Members) > 512 {
		return reject()
	}
	var members []json.RawMessage
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		return reject()
	}
	addresses, keys := map[string]bool{}, map[string]bool{}
	for i, member := range result.Members {
		fields, err := membershipObjectOptional(members[i], []string{"id", "address", "role", "key_fingerprint"}, "enrollment_origin")
		if err != nil || !membershipDecimal(fields["id"], member.ID) || (i > 0 && result.Members[i-1].ID >= member.ID) {
			return reject()
		}
		address, err := identity.NormalizeMemberAddress(member.Address)
		if err != nil || address != member.Address || addresses[address] || !validCatalogDigest(member.KeyFingerprint) || keys[member.KeyFingerprint] {
			return reject()
		}
		if raw := fields["enrollment_origin"]; raw != nil {
			if member.EnrollmentOrigin == "" || !canonicalEnrollmentOrigin(member.EnrollmentOrigin) {
				return reject()
			}
		}
		// These are Role.String values of the pinned go-dqlite implementation.
		switch member.Role {
		case "voter", "stand-by", "spare":
		default:
			return reject()
		}
		addresses[address], keys[member.KeyFingerprint] = true, true
	}
	return result, nil
}

func canonicalEnrollmentOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") || u.Host == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	address, err := identity.NormalizeMemberAddress(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	canonical := "https://" + address
	if port == "443" {
		canonical = "https://" + host
		if strings.Contains(host, ":") {
			canonical = "https://[" + host + "]"
		}
	}
	return origin == canonical
}

// CanonicalEnrollmentOrigin is shared by enrollment clients and the joining
// command before an origin is frozen into the durable member target.
func CanonicalEnrollmentOrigin(origin string) bool { return canonicalEnrollmentOrigin(origin) }

func membershipDecimal(data []byte, value uint64) bool {
	var text string
	return value > 0 && json.Unmarshal(data, &text) == nil && text == strconv.FormatUint(value, 10)
}

// Both the outer snapshot and every member reject duplicate, missing, unknown
// or differently-cased names before Go's permissive struct decoder is used.
func membershipObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	return membershipObjectOptional(data, names)
}

func membershipObjectOptional(data []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	invalid := errors.New("membership requires an exact JSON object")
	fields := make(map[string]json.RawMessage, len(required)+len(optional))
	for _, name := range required {
		fields[name] = nil
	}
	for _, name := range optional {
		fields[name] = nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalid
		}
		name, ok := token.(string)
		if !ok {
			return nil, invalid
		}
		previous, known := fields[name]
		if !known || previous != nil {
			return nil, invalid
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalid
		}
		fields[name] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, invalid
	}
	for _, name := range required {
		if fields[name] == nil {
			return nil, invalid
		}
	}
	return fields, nil
}

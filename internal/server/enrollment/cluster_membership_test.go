package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestClusterMembershipPeerKeyMustBelongToAuthenticatedDirectory(t *testing.T) {
	var members []ClusterMember
	var clusterID, pin string
	f := transportPolicyEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != ClusterMembershipPath {
			t.Errorf("unexpected membership request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(ClusterMembershipSnapshot{
			ClusterID: clusterID, CAFingerprint: pin, Protocol: proto.ClusterProtocolVersion,
			SourceRevision: 1, Members: members,
		})
	}))
	clusterID, pin = f.cluster, f.pin
	serverKey := sha256.Sum256(f.server.Certificate().RawSubjectPublicKeyInfo)
	peerKey := hex.EncodeToString(serverKey[:])
	members = []ClusterMember{{ID: 1, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: peerKey}}
	cert, key := catalogMember(t, f)
	client := f.client(t)
	got, actualPeer, err := client.FetchClusterMembershipWithPeerKey(t.Context(), cert, key)
	if err != nil || actualPeer != peerKey || len(got.Members) != 1 {
		t.Fatalf("authenticated serving key lost: %+v %q %v", got, actualPeer, err)
	}
	members[0].KeyFingerprint = strings.Repeat("0", 64)
	if got, actualPeer, err := client.FetchClusterMembershipWithPeerKey(t.Context(), cert, key); err == nil || actualPeer != "" || len(got.Members) != 0 {
		t.Fatalf("unlisted serving member accepted: %+v %q %v", got, actualPeer, err)
	}
}

func TestClusterMembershipClientStrictSnapshot(t *testing.T) {
	cases := []string{"valid_unjoined", "limit", "foreign_cluster", "foreign_ca", "protocol", "null_members", "empty_members", "too_many", "null_member", "zero_id", "numeric_id", "negative_id", "overflow_id", "leading_zero_id", "unsorted_ids", "duplicate_id", "duplicate_address", "duplicate_key", "uppercase_key", "invalid_key", "noncanonical_host", "noncanonical_port", "invalid_address", "invalid_role", "legacy_role", "zero_revision", "numeric_revision", "negative_revision", "overflow_revision", "leading_zero_revision", "null_revision", "unknown", "case_alias", "duplicate", "escaped_duplicate", "unknown_member", "duplicate_member_field", "escaped_duplicate_member_field", "member_case_alias", "trailing", "root_null", "root_array", "oversize", "truncated", "redirect", "remote_error", "canceled", "wrong_pin", "wrong_key"}
	for _, name := range []string{"cluster_id", "ca_fingerprint", "protocol", "source_revision", "members"} {
		cases = append(cases, "missing_"+name)
	}
	for _, name := range []string{"id", "address", "role", "key_fingerprint"} {
		cases = append(cases, "missing_member_"+name)
	}
	cases = append(cases, "valid_origin", "origin_null", "origin_empty", "origin_http", "origin_noncanonical", "origin_path", "origin_userinfo", "origin_query", "origin_bad_host")
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			var payload []byte
			f := transportPolicyEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != ClusterMembershipPath || r.ContentLength != 0 || r.Header.Get("Authorization") != "" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
					t.Error("unexpected discovery request")
				}
				switch name {
				case "redirect":
					w.Header().Set("Location", "https://example.invalid/private")
					w.WriteHeader(307)
					return
				case "remote_error":
					w.WriteHeader(403)
					_, _ = w.Write([]byte("private-response-marker"))
					return
				case "truncated":
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+10))
				}
				_, _ = w.Write(payload)
			}))
			member := func(id, address, role, key string) map[string]any {
				return map[string]any{"id": id, "address": address, "role": role, "key_fingerprint": key}
			}
			members := []map[string]any{
				member("2", "127.0.0.1:9010", "voter", fmt.Sprintf("%064x", 1)),
				member("10", "node.example:9011", "stand-by", fmt.Sprintf("%064x", 2)),
				member("18446744073709551615", "[2001:db8::1]:9012", "spare", fmt.Sprintf("%064x", 3)),
			}
			body := map[string]any{"cluster_id": f.cluster, "ca_fingerprint": f.pin, "protocol": proto.ClusterProtocolVersion, "source_revision": "18446744073709551615", "members": members}
			switch name {
			case "limit", "too_many":
				count := 512
				if name == "too_many" {
					count++
				}
				members = nil
				for i := 0; i < count; i++ {
					members = append(members, member(fmt.Sprint(i+1), fmt.Sprintf("127.0.0.1:%d", 9000+i), "voter", fmt.Sprintf("%064x", i)))
				}
				body["members"] = members
			case "foreign_cluster":
				body["cluster_id"] = strings.Repeat("b", 32)
			case "foreign_ca":
				body["ca_fingerprint"] = strings.Repeat("b", 64)
			case "protocol":
				body["protocol"] = proto.ClusterProtocolVersion + 1
			case "null_members":
				body["members"] = nil
			case "empty_members":
				body["members"] = []any{}
			case "null_member":
				body["members"] = []any{nil}
			case "zero_id":
				members[0]["id"] = "0"
			case "numeric_id":
				members[0]["id"] = 2
			case "negative_id":
				members[0]["id"] = "-1"
			case "overflow_id":
				members[0]["id"] = "18446744073709551616"
			case "leading_zero_id":
				members[0]["id"] = "02"
			case "unsorted_ids":
				members[0]["id"], members[1]["id"] = members[1]["id"], members[0]["id"]
			case "duplicate_id":
				members[1]["id"] = members[0]["id"]
			case "duplicate_address":
				members[1]["address"] = members[0]["address"]
			case "duplicate_key":
				members[1]["key_fingerprint"] = members[0]["key_fingerprint"]
			case "uppercase_key":
				members[0]["key_fingerprint"] = strings.Repeat("A", 64)
			case "invalid_key":
				members[0]["key_fingerprint"] = "../key"
			case "noncanonical_host":
				members[1]["address"] = "NODE.EXAMPLE.:9011"
			case "noncanonical_port":
				members[0]["address"] = "127.0.0.1:09010"
			case "invalid_address":
				members[0]["address"] = "0.0.0.0:9010"
			case "invalid_role":
				members[0]["role"] = "leader"
			case "legacy_role":
				members[1]["role"] = "standby"
			case "valid_origin":
				members[0]["enrollment_origin"] = "https://public.example:9443"
			case "origin_null":
				members[0]["enrollment_origin"] = nil
			case "origin_empty":
				members[0]["enrollment_origin"] = ""
			case "origin_http":
				members[0]["enrollment_origin"] = "http://public.example:9443"
			case "origin_noncanonical":
				members[0]["enrollment_origin"] = "https://PUBLIC.example:09443"
			case "origin_path":
				members[0]["enrollment_origin"] = "https://public.example:9443/path"
			case "origin_userinfo":
				members[0]["enrollment_origin"] = "https://user@public.example:9443"
			case "origin_query":
				members[0]["enrollment_origin"] = "https://public.example:9443?x=1"
			case "origin_bad_host":
				members[0]["enrollment_origin"] = "https://0.0.0.0:9443"
			case "zero_revision":
				body["source_revision"] = "0"
			case "numeric_revision":
				body["source_revision"] = 1
			case "negative_revision":
				body["source_revision"] = "-1"
			case "overflow_revision":
				body["source_revision"] = "18446744073709551616"
			case "leading_zero_revision":
				body["source_revision"] = "01"
			case "null_revision":
				body["source_revision"] = nil
			case "unknown":
				body["extra"] = true
			case "case_alias":
				body["Cluster_ID"] = body["cluster_id"]
				delete(body, "cluster_id")
			case "unknown_member":
				members[0]["extra"] = true
			case "member_case_alias":
				members[0]["ID"] = members[0]["id"]
				delete(members[0], "id")
			}
			if strings.HasPrefix(name, "missing_member_") {
				delete(members[0], strings.TrimPrefix(name, "missing_member_"))
			} else if strings.HasPrefix(name, "missing_") {
				delete(body, strings.TrimPrefix(name, "missing_"))
			}
			if name == "duplicate_member_field" || name == "escaped_duplicate_member_field" {
				raw, err := json.Marshal(members[0])
				if err != nil {
					t.Fatal(err)
				}
				suffix := `,"id":"2"}`
				if name == "escaped_duplicate_member_field" {
					suffix = `,"\u0069d":"2"}`
				}
				body["members"] = []json.RawMessage{append(raw[:len(raw)-1], []byte(suffix)...)}
			}
			var err error
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "duplicate":
				payload = append(payload[:len(payload)-1], []byte(`,"protocol":1}`)...)
			case "escaped_duplicate":
				payload = append(payload[:len(payload)-1], []byte(`,"pro\u0074ocol":1}`)...)
			case "trailing":
				payload = append(payload, []byte(" {}")...)
			case "root_null":
				payload = []byte("null")
			case "root_array":
				payload = []byte("[]")
			case "oversize":
				payload = []byte(strings.Repeat("x", (128<<10)+1))
			}
			cert, key := catalogMember(t, f)
			client := f.client(t)
			if name == "wrong_pin" {
				client, err = NewClient(f.server.URL+certificatePath, f.cluster, strings.Repeat("b", 64), proto.ClusterProtocolVersion)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
			}
			if name == "wrong_key" {
				_, key = catalogMember(t, f)
			}
			ctx := t.Context()
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := client.FetchClusterMembership(ctx, cert, key)
			if name == "valid_unjoined" || name == "valid_origin" || name == "limit" {
				var expected ClusterMembershipSnapshot
				if err := json.Unmarshal(payload, &expected); err != nil {
					t.Fatal(err)
				}
				if err != nil || !reflect.DeepEqual(got, expected) {
					t.Fatalf("valid discovery: %+v %v", got, err)
				}
				csr, err := identity.NewMemberCSR(key)
				if err != nil {
					t.Fatal(err)
				}
				own, err := identity.CSRKeyFingerprint(csr)
				if err != nil {
					t.Fatal(err)
				}
				for _, member := range got.Members {
					if member.KeyFingerprint == own {
						t.Fatal("fixture accidentally made requester a Raft member")
					}
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, ClusterMembershipSnapshot{}) {
				t.Fatalf("invalid discovery returned partial members: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("source body leaked")
			}
		})
	}
}

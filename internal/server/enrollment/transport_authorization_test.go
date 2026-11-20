package enrollment

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func authorizationServerKey(t *testing.T, f *fixture) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(f.server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(digest[:])
}

func TestTransportAuthorizationReceiptStrictAuthenticatedOwner(t *testing.T) {
	cases := []string{"valid", "foreign_cluster", "foreign_ca", "protocol", "wrong_receipt_member", "same_ca_impersonation", "invalid_expected", "unknown", "case_alias", "duplicate", "escaped_duplicate", "trailing", "null", "array", "oversize", "truncated", "remote_error", "redirect", "canceled", "wrong_pin", "wrong_key"}
	for _, field := range []string{"cluster_id", "ca_fingerprint", "protocol", "member_key", "source_revision", "local_revision"} {
		cases = append(cases, "missing_"+field)
	}
	for _, field := range []string{"source_revision", "local_revision"} {
		for _, fault := range []string{"zero", "numeric", "negative", "overflow", "leading_zero", "null"} {
			cases = append(cases, field+"_"+fault)
		}
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			var payload []byte
			var requests atomic.Int32
			f := transportPolicyEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != TransportAuthorizationPath || r.ContentLength != 0 || r.Header.Get("Authorization") != "" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
					t.Error("unexpected member authorization request")
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
			cert, key := catalogMember(t, f)
			expected := authorizationServerKey(t, f)
			body := map[string]any{"cluster_id": f.cluster, "ca_fingerprint": f.pin, "protocol": proto.ClusterProtocolVersion, "member_key": expected, "source_revision": "18446744073709551615", "local_revision": "9007199254740993"}
			switch name {
			case "foreign_cluster":
				body["cluster_id"] = strings.Repeat("b", 32)
			case "foreign_ca":
				body["ca_fingerprint"] = strings.Repeat("b", 64)
			case "protocol":
				body["protocol"] = proto.ClusterProtocolVersion + 1
			case "wrong_receipt_member":
				body["member_key"] = strings.Repeat("b", 64)
			case "same_ca_impersonation":
				// B has its own key and a certificate from A's CA. A cannot claim B's
				// acknowledgement merely by putting B's fingerprint in its JSON response.
				_, otherKey := catalogMember(t, f)
				csr, err := identity.NewMemberCSR(otherKey)
				if err != nil {
					t.Fatal(err)
				}
				expected, err = identity.CSRKeyFingerprint(csr)
				if err != nil {
					t.Fatal(err)
				}
				body["member_key"] = expected
			case "invalid_expected":
				expected = "../member"
			case "unknown":
				body["extra"] = true
			case "case_alias":
				body["Member_Key"] = body["member_key"]
				delete(body, "member_key")
			}
			if strings.HasPrefix(name, "missing_") {
				delete(body, strings.TrimPrefix(name, "missing_"))
			}
			for _, field := range []string{"source_revision", "local_revision"} {
				switch strings.TrimPrefix(name, field+"_") {
				case "zero":
					body[field] = "0"
				case "numeric":
					body[field] = 1
				case "negative":
					body[field] = "-1"
				case "overflow":
					body[field] = "18446744073709551616"
				case "leading_zero":
					body[field] = "01"
				case "null":
					if name == field+"_null" {
						body[field] = nil
					}
				}
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
			case "null":
				payload = []byte("null")
			case "array":
				payload = []byte("[]")
			case "oversize":
				payload = []byte(strings.Repeat("x", (8<<10)+1))
			}
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
			got, err := client.FetchTransportAuthorization(ctx, cert, key, expected)
			if name == "valid" {
				var wanted TransportAuthorizationReceipt
				if err := json.Unmarshal(payload, &wanted); err != nil {
					t.Fatal(err)
				}
				if err != nil || !reflect.DeepEqual(got, wanted) {
					t.Fatalf("valid receipt: %+v %v", got, err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, TransportAuthorizationReceipt{}) {
				t.Fatalf("invalid receipt returned data: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("remote response leaked")
			}
			if name == "same_ca_impersonation" && requests.Load() != 0 {
				t.Fatal("peer binding checked only after HTTP request")
			}
		})
	}
}

func TestTransportAuthorizationPeerBindingDoesNotMutateOtherRequests(t *testing.T) {
	var receipt TransportAuthorizationReceipt
	var policy TransportPolicySnapshot
	f := transportPolicyEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case TransportAuthorizationPath:
			_ = json.NewEncoder(w).Encode(receipt)
		case TransportPolicyPath:
			_ = json.NewEncoder(w).Encode(policy)
		case CatalogPackagesPath:
			_ = json.NewEncoder(w).Encode(CatalogPackageIndex{ClusterID: receipt.ClusterID, Protocol: receipt.Protocol, Digests: []string{}})
		default:
			http.NotFound(w, r)
		}
	}))
	cert, key := catalogMember(t, f)
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	own, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	receipt = TransportAuthorizationReceipt{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: proto.ClusterProtocolVersion, MemberKey: authorizationServerKey(t, f), SourceRevision: 7, LocalRevision: 2}
	policy = TransportPolicySnapshot{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: proto.ClusterProtocolVersion, SourceRevision: 7, AllowedKeys: []string{own}, RevokedKeys: []string{}}
	client := f.client(t)
	if _, err := client.FetchTransportAuthorization(t.Context(), cert, key, strings.Repeat("0", 64)); err == nil {
		t.Fatal("unexpected owner accepted")
	}
	if got, err := client.FetchTransportAuthorization(t.Context(), cert, key, receipt.MemberKey); err != nil || got != receipt {
		t.Fatalf("failed peer check mutated shared client: %+v %v", got, err)
	}
	if got, err := client.FetchTransportPolicy(t.Context(), cert, key); err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("policy request changed: %+v %v", got, err)
	}
	if got, err := client.FetchCatalogIndex(t.Context(), cert, key); err != nil || got.Digests == nil || len(got.Digests) != 0 {
		t.Fatalf("catalog request changed: %+v %v", got, err)
	}
}

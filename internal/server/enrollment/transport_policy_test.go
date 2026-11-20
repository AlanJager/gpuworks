package enrollment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func transportPolicyEndpoint(t *testing.T, handler http.Handler) *fixture {
	t.Helper()
	f := endpoint(t, handler)
	config := f.server.TLS.Clone()
	logger := f.server.Config.ErrorLog
	f.server.Close()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(f.ca) {
		t.Fatal("invalid fixture CA")
	}
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.ClientCAs = roots
	f.server = httptest.NewUnstartedServer(handler)
	f.server.Config.ErrorLog = logger
	f.server.TLS = config
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func TestTransportPolicyClientStrictSnapshot(t *testing.T) {
	cases := []string{"valid", "limit", "foreign_cluster", "foreign_ca", "protocol", "zero_revision", "numeric_revision", "negative_revision", "overflow_revision", "leading_zero_revision", "null_revision", "null_allowed", "empty_allowed", "null_revoked", "unsorted_allowed", "duplicate_allowed", "unsorted_revoked", "duplicate_revoked", "uppercase", "invalid_digest", "overlap", "missing_requester", "too_many_allowed", "too_many_total", "unknown", "case_alias", "duplicate_field", "escaped_duplicate", "trailing", "root_null", "root_array", "oversize", "redirect", "remote_error", "truncated", "canceled", "wrong_pin", "wrong_key"}
	for _, name := range []string{"cluster_id", "ca_fingerprint", "protocol", "source_revision", "allowed_keys", "revoked_keys"} {
		cases = append(cases, "missing_"+name)
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			var payload []byte
			f := transportPolicyEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != TransportPolicyPath || r.ContentLength != 0 || r.Header.Get("Authorization") != "" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
					t.Error("snapshot request did not use the exact empty member-authenticated endpoint")
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
			csr, err := identity.NewMemberCSR(key)
			if err != nil {
				t.Fatal(err)
			}
			own, err := identity.CSRKeyFingerprint(csr)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"cluster_id": f.cluster, "ca_fingerprint": f.pin, "protocol": proto.ClusterProtocolVersion, "source_revision": "18446744073709551615", "allowed_keys": []string{own}, "revoked_keys": []string{}}
			switch name {
			case "limit", "too_many_allowed", "too_many_total":
				count := 511
				if name == "too_many_allowed" {
					count = 512
				}
				keys := []string{own}
				for i := 0; i < count; i++ {
					keys = append(keys, fmt.Sprintf("%064x", i))
				}
				sort.Strings(keys)
				body["allowed_keys"] = keys
				if name == "too_many_total" {
					body["revoked_keys"] = []string{strings.Repeat("f", 64)}
				}
			case "foreign_cluster":
				body["cluster_id"] = strings.Repeat("b", 32)
			case "foreign_ca":
				body["ca_fingerprint"] = strings.Repeat("b", 64)
			case "protocol":
				body["protocol"] = proto.ClusterProtocolVersion + 1
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
			case "null_allowed":
				body["allowed_keys"] = nil
			case "empty_allowed":
				body["allowed_keys"] = []string{}
			case "null_revoked":
				body["revoked_keys"] = nil
			case "unsorted_allowed":
				body["allowed_keys"] = []string{own, strings.Repeat("0", 64)}
			case "duplicate_allowed":
				body["allowed_keys"] = []string{own, own}
			case "unsorted_revoked":
				body["revoked_keys"] = []string{strings.Repeat("f", 64), strings.Repeat("0", 64)}
			case "duplicate_revoked":
				body["revoked_keys"] = []string{strings.Repeat("0", 64), strings.Repeat("0", 64)}
			case "uppercase":
				body["allowed_keys"] = []string{strings.ToUpper(own)}
			case "invalid_digest":
				body["revoked_keys"] = []string{"../private"}
			case "overlap":
				body["revoked_keys"] = []string{own}
			case "missing_requester":
				body["allowed_keys"] = []string{strings.Repeat("0", 64)}
			case "unknown":
				body["extra"] = true
			case "case_alias":
				body["Cluster_ID"] = body["cluster_id"]
				delete(body, "cluster_id")
			}
			if strings.HasPrefix(name, "missing_") && name != "missing_requester" {
				delete(body, strings.TrimPrefix(name, "missing_"))
			}
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "duplicate_field":
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
			got, err := client.FetchTransportPolicy(ctx, cert, key)
			if name == "valid" || name == "limit" {
				var expected TransportPolicySnapshot
				if decodeErr := json.Unmarshal(payload, &expected); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if err != nil || !reflect.DeepEqual(got, expected) {
					t.Fatalf("valid snapshot: %+v %v", got, err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, TransportPolicySnapshot{}) {
				t.Fatalf("rejected snapshot returned result: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("remote body leaked through error")
			}
		})
	}
}

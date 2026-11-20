package enrollment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const quorumDiscoveryLiteral = `{"cluster_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ca_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol":1,"nonce":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","members":[{"id":"11","address":"leader.example:9010","role":"voter","key_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","enrollment_origin":"https://leader.example:9443"}]}`

func TestQuorumDiscoveryLiteralAndRoundTrip(t *testing.T) {
	got, err := DecodeClusterQuorumDiscovery([]byte(quorumDiscoveryLiteral))
	want := ClusterQuorumDiscovery{ClusterID: strings.Repeat("a", 32), CAFingerprint: strings.Repeat("b", 64), Protocol: 1, Nonce: strings.Repeat("c", 64), Members: []ClusterMember{{ID: 11, Address: "leader.example:9010", Role: "voter", KeyFingerprint: strings.Repeat("d", 64), EnrollmentOrigin: "https://leader.example:9443"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("literal mismatch: %+v %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || string(encoded) != quorumDiscoveryLiteral {
		t.Fatalf("wire mismatch: %s %v", encoded, err)
	}
}

func TestQuorumDiscoveryRejectsUnboundedOrAmbiguousHints(t *testing.T) {
	for _, name := range []string{"missing nonce", "duplicate", "unknown proof", "case", "null members", "bad nonce", "bad cluster", "bad protocol", "bad origin", "duplicate member key", "oversize"} {
		t.Run(name, func(t *testing.T) {
			payload := quorumDiscoveryLiteral
			switch name {
			case "missing nonce":
				payload = strings.Replace(payload, `"nonce":"`+strings.Repeat("c", 64)+`",`, "", 1)
			case "duplicate":
				payload = strings.Replace(payload, `"protocol":1`, `"protocol":1,"protocol":1`, 1)
			case "unknown proof":
				payload = strings.Replace(payload, `"protocol":1`, `"protocol":1,"index":"2"`, 1)
			case "case":
				payload = strings.Replace(payload, `"nonce":`, `"Nonce":`, 1)
			case "null members":
				payload = payload[:strings.Index(payload, `"members":`)] + `"members":null}`
			case "bad nonce":
				payload = strings.Replace(payload, strings.Repeat("c", 64), strings.Repeat("C", 64), 1)
			case "bad cluster":
				payload = strings.Replace(payload, strings.Repeat("a", 32), strings.Repeat("a", 31), 1)
			case "bad protocol":
				payload = strings.Replace(payload, `"protocol":1`, `"protocol":2`, 1)
			case "bad origin":
				payload = strings.Replace(payload, "https://leader.example:9443", "http://leader.example:9443", 1)
			case "duplicate member key":
				payload = strings.Replace(payload, `}]}`, `},{"id":"12","address":"other.example:9010","role":"spare","key_fingerprint":"`+strings.Repeat("d", 64)+`"}]}`, 1)
			case "oversize":
				payload = strings.Repeat("x", (512<<10)+1)
			}
			got, err := DecodeClusterQuorumDiscovery([]byte(payload))
			if err == nil || !reflect.DeepEqual(got, ClusterQuorumDiscovery{}) {
				t.Fatalf("partial/accepted hints: %+v %v", got, err)
			}
		})
	}
}

package enrollment

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestClusterQuorumFactsRequestLiteralAndBound(t *testing.T) {
	if ClusterQuorumFactsPath != "/_cluster/quorum-facts" {
		t.Fatal("wrong facts endpoint", ClusterQuorumFactsPath)
	}
	for _, which := range []string{"literal", "exact-4KiB"} {
		t.Run(which, func(t *testing.T) {
			data := []byte(quorumFactsRequestLiteral)
			if which == "exact-4KiB" {
				data = append(data, bytes.Repeat([]byte(" "), (4<<10)-len(data))...)
			}
			request, err := DecodeClusterQuorumFactsRequest(data)
			if err != nil || request.Nonce != strings.Repeat("c", 64) {
				t.Fatalf("literal nonce request rejected: %+v / %v", request, err)
			}
			encoded, err := json.Marshal(request)
			if err != nil || string(encoded) != quorumFactsRequestLiteral {
				t.Fatalf("request wire roundtrip changed: %s / %v", encoded, err)
			}
		})
	}
}

func TestClusterQuorumFactsRequestRejectsMalformedWire(t *testing.T) {
	cases := map[string]string{
		"missing": `{}`, "unknown": `{"nonce":"` + strings.Repeat("c", 64) + `","secret":"hidden"}`,
		"case-alias":        strings.Replace(quorumFactsRequestLiteral, "nonce", "Nonce", 1),
		"duplicate":         strings.TrimSuffix(quorumFactsRequestLiteral, "}") + `,"nonce":"` + strings.Repeat("c", 64) + `"}`,
		"escaped-duplicate": strings.TrimSuffix(quorumFactsRequestLiteral, "}") + `,"n\u006fnce":"` + strings.Repeat("c", 64) + `"}`,
		"null-nonce":        `{"nonce":null}`, "numeric-nonce": `{"nonce":7}`,
		"uppercase": `{"nonce":"` + strings.Repeat("A", 64) + `"}`, "short": `{"nonce":"` + strings.Repeat("c", 63) + `"}`,
		"nonhex": `{"nonce":"` + strings.Repeat("g", 64) + `"}`, "trailing": quorumFactsRequestLiteral + `{}`,
		"null-root": `null`, "array-root": `[]`, "truncated": strings.TrimSuffix(quorumFactsRequestLiteral, "}"),
		"over-4KiB": quorumFactsRequestLiteral + strings.Repeat(" ", (4<<10)+1-len(quorumFactsRequestLiteral)),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := DecodeClusterQuorumFactsRequest([]byte(payload))
			if err == nil || result != (ClusterQuorumFactsRequest{}) {
				t.Fatalf("bad request accepted: %+v / %v", result, err)
			}
		})
	}
}

func TestClusterQuorumFactsLiteralRoundTripAndPhases(t *testing.T) {
	for _, phase := range []string{"serving", "freezing", "frozen", "drained", "upgrading", "recovering", "legacy"} {
		t.Run(phase, func(t *testing.T) {
			payload := quorumFactsLiteral
			if phase == "legacy" {
				payload = strings.Replace(payload, `"legacy":false`, `"legacy":true`, 1)
				payload = strings.Replace(payload, `"phase":"frozen"`, `"phase":""`, 1)
				payload = strings.Replace(payload, `"phase_revision":"37"`, `"phase_revision":"0"`, 1)
			} else {
				payload = strings.Replace(payload, `"phase":"frozen"`, `"phase":"`+phase+`"`, 1)
			}
			facts, err := DecodeClusterQuorumFacts([]byte(payload))
			if err != nil || facts.ClusterID != strings.Repeat("a", 32) || facts.CAFingerprint != strings.Repeat("b", 64) || facts.Protocol != 1 || facts.Nonce != strings.Repeat("c", 64) || facts.Term != 7 || facts.Index != 23 || facts.ConfigurationIndex != 20 || facts.LeaderID != 11 || len(facts.Members) != 3 {
				t.Fatalf("literal facts rejected or changed: %+v / %v", facts, err)
			}
			if phase == "legacy" {
				if !facts.Legacy || facts.Phase != "" || facts.PhaseRevision != 0 {
					t.Fatal("legacy observation changed", facts)
				}
			} else if facts.Legacy || facts.Phase != phase || facts.PhaseRevision != 37 {
				t.Fatal("current observation changed", facts)
			}
			for i, member := range facts.Members {
				if member.ID != uint64(11+i) || member.Role != []string{"voter", "stand-by", "spare"}[i] || member.KeyFingerprint != strings.Repeat([]string{"d", "e", "f"}[i], 64) {
					t.Fatal("tuple changed", member)
				}
			}
			encoded, err := json.Marshal(facts)
			if err != nil || string(encoded) != payload {
				t.Fatalf("exact required wire keys/value/optional origin changed: %s / %v", encoded, err)
			}
		})
	}
}

func TestClusterQuorumFactsAcceptsCapacityAndUnsignedBounds(t *testing.T) {
	for _, which := range []string{"exact-512KiB", "members-512", "uint64-max"} {
		t.Run(which, func(t *testing.T) {
			data := []byte(quorumFactsLiteral)
			switch which {
			case "exact-512KiB":
				data = append(data, bytes.Repeat([]byte(" "), (512<<10)-len(data))...)
			case "members-512":
				data = quorumFactsCapacity(t, 512)
			case "uint64-max":
				fields := quorumFactsFields(t)
				for _, name := range []string{"phase_revision", "term", "index", "configuration_index"} {
					quorumFactsSet(fields, name, `"18446744073709551615"`)
				}
				data = quorumFactsJSON(t, fields)
			}
			facts, err := DecodeClusterQuorumFacts(data)
			if err != nil {
				t.Fatal("valid boundary refused", which, err)
			}
			if which == "members-512" && len(facts.Members) != 512 {
				t.Fatal("membership truncated", len(facts.Members))
			}
			if which == "uint64-max" && (facts.Term != ^uint64(0) || facts.Index != ^uint64(0) || facts.ConfigurationIndex != ^uint64(0) || facts.PhaseRevision != ^uint64(0)) {
				t.Fatal("unsigned decimal narrowed", facts)
			}
		})
	}
}

func TestClusterQuorumFactsRequiredFieldsCannotBeMissingOrNull(t *testing.T) {
	for _, name := range []string{"cluster_id", "ca_fingerprint", "protocol", "nonce", "legacy", "phase", "phase_revision", "term", "index", "configuration_index", "leader_id", "members"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				fields := quorumFactsFields(t)
				if mode == "missing" {
					delete(fields, name)
				} else {
					fields[name] = json.RawMessage("null")
				}
				assertQuorumFactsRejected(t, quorumFactsJSON(t, fields))
			})
		}
	}
}

func assertQuorumFactsRejected(t *testing.T, data []byte) {
	t.Helper()
	facts, err := DecodeClusterQuorumFacts(data)
	if err == nil || !reflect.DeepEqual(facts, ClusterQuorumFacts{}) {
		t.Fatalf("malformed wire returned facts: %+v / %v", facts, err)
	}
}

func TestClusterQuorumFactsRejectsMalformedObservation(t *testing.T) {
	cases := []string{"unknown", "case-alias", "duplicate", "escaped-duplicate", "trailing", "root-array", "truncated", "over-512KiB", "cluster-uppercase", "cluster-short", "cluster-nonhex", "ca-uppercase", "protocol-other", "protocol-string", "nonce-uppercase", "bool-string", "bool-number", "phase-unknown", "revision-zero", "numeric-index", "negative-index", "leading-zero-index", "overflow-index", "config-after-index", "legacy-phase", "legacy-revision-positive", "legacy-revision-leading-zero", "legacy-revision-number", "legacy-null-phase", "nonlegacy-empty-phase", "leader-absent", "leader-standby", "members-513", "members-empty", "member-null", "member-unsorted", "member-duplicate-address", "member-duplicate-key", "member-role", "member-duplicate-field", "member-unknown-field"}
	for _, which := range cases {
		t.Run(which, func(t *testing.T) {
			fields := quorumFactsFields(t)
			var raw []byte
			switch which {
			case "unknown":
				fields["source_revision"] = json.RawMessage(`"1"`)
			case "case-alias":
				fields["Term"] = fields["term"]
				delete(fields, "term")
			case "duplicate":
				raw = []byte(strings.TrimSuffix(quorumFactsLiteral, "}") + `,"term":"7"}`)
			case "escaped-duplicate":
				raw = []byte(strings.TrimSuffix(quorumFactsLiteral, "}") + `,"t\u0065rm":"7"}`)
			case "trailing":
				raw = []byte(quorumFactsLiteral + `{}`)
			case "root-array":
				raw = []byte(`[]`)
			case "truncated":
				raw = []byte(strings.TrimSuffix(quorumFactsLiteral, "}"))
			case "over-512KiB":
				raw = []byte(quorumFactsLiteral + strings.Repeat(" ", (512<<10)+1-len(quorumFactsLiteral)))
			case "cluster-uppercase":
				quorumFactsSet(fields, "cluster_id", `"`+strings.Repeat("A", 32)+`"`)
			case "cluster-short":
				quorumFactsSet(fields, "cluster_id", `"`+strings.Repeat("a", 31)+`"`)
			case "cluster-nonhex":
				quorumFactsSet(fields, "cluster_id", `"`+strings.Repeat("g", 32)+`"`)
			case "ca-uppercase":
				quorumFactsSet(fields, "ca_fingerprint", `"`+strings.Repeat("B", 64)+`"`)
			case "protocol-other":
				quorumFactsSet(fields, "protocol", `2`)
			case "protocol-string":
				quorumFactsSet(fields, "protocol", `"1"`)
			case "nonce-uppercase":
				quorumFactsSet(fields, "nonce", `"`+strings.Repeat("C", 64)+`"`)
			case "bool-string":
				quorumFactsSet(fields, "legacy", `"false"`)
			case "bool-number":
				quorumFactsSet(fields, "legacy", `0`)
			case "phase-unknown":
				quorumFactsSet(fields, "phase", `"paused"`)
			case "revision-zero":
				quorumFactsSet(fields, "phase_revision", `"0"`)
			case "numeric-index":
				quorumFactsSet(fields, "index", `23`)
			case "negative-index":
				quorumFactsSet(fields, "index", `"-23"`)
			case "leading-zero-index":
				quorumFactsSet(fields, "index", `"023"`)
			case "overflow-index":
				quorumFactsSet(fields, "index", `"18446744073709551616"`)
			case "config-after-index":
				quorumFactsSet(fields, "configuration_index", `"24"`)
			case "nonlegacy-empty-phase":
				quorumFactsSet(fields, "phase", `""`)
			case "leader-absent":
				quorumFactsSet(fields, "leader_id", `"99"`)
			case "leader-standby":
				quorumFactsSet(fields, "leader_id", `"12"`)
			case "members-513":
				raw = quorumFactsCapacity(t, 513)
			case "members-empty":
				quorumFactsSet(fields, "members", `[]`)
			case "member-null":
				quorumFactsSet(fields, "members", `[null]`)
			case "member-duplicate-field":
				raw = []byte(strings.Replace(quorumFactsLiteral, `"id":"11"`, `"id":"11","id":"11"`, 1))
			case "member-unknown-field":
				raw = []byte(strings.Replace(quorumFactsLiteral, `"id":"11"`, `"id":"11","private_key":"secret"`, 1))
			case "member-unsorted", "member-duplicate-address", "member-duplicate-key", "member-role":
				members := quorumFactsMembers(t, fields)
				switch which {
				case "member-unsorted":
					members[0], members[1] = members[1], members[0]
				case "member-duplicate-address":
					members[1]["address"] = members[0]["address"]
				case "member-duplicate-key":
					members[1]["key_fingerprint"] = members[0]["key_fingerprint"]
				case "member-role":
					members[1]["role"] = json.RawMessage(`"standby"`)
				}
				quorumFactsSetMembers(t, fields, members)
			default:
				quorumFactsSet(fields, "legacy", `true`)
				quorumFactsSet(fields, "phase", `""`)
				quorumFactsSet(fields, "phase_revision", `"0"`)
				switch which {
				case "legacy-phase":
					quorumFactsSet(fields, "phase", `"frozen"`)
				case "legacy-revision-positive":
					quorumFactsSet(fields, "phase_revision", `"1"`)
				case "legacy-revision-leading-zero":
					quorumFactsSet(fields, "phase_revision", `"00"`)
				case "legacy-revision-number":
					quorumFactsSet(fields, "phase_revision", `0`)
				case "legacy-null-phase":
					quorumFactsSet(fields, "phase", `null`)
				default:
					t.Fatal("unhandled fixture variant", which)
				}
			}
			if raw == nil {
				raw = quorumFactsJSON(t, fields)
			}
			assertQuorumFactsRejected(t, raw)
		})
	}
	for _, field := range []string{"term", "index", "configuration_index", "leader_id"} {
		t.Run("zero-"+field, func(t *testing.T) {
			fields := quorumFactsFields(t)
			fields[field] = json.RawMessage(`"0"`)
			assertQuorumFactsRejected(t, quorumFactsJSON(t, fields))
		})
	}
}

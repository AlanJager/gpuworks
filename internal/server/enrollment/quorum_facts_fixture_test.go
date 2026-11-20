package enrollment

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const quorumFactsRequestLiteral = `{"nonce":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`
const quorumFactsLiteral = `{"cluster_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ca_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol":1,"nonce":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","legacy":false,"phase":"frozen","phase_revision":"37","term":"7","index":"23","configuration_index":"20","leader_id":"11","members":[{"id":"11","address":"leader.example:9010","role":"voter","key_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","enrollment_origin":"https://leader.example:9443"},{"id":"12","address":"standby.example:9010","role":"stand-by","key_fingerprint":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},{"id":"13","address":"spare.example:9010","role":"spare","key_fingerprint":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}]}`

func quorumFactsFields(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(quorumFactsLiteral), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func quorumFactsJSON(t *testing.T, fields map[string]json.RawMessage) []byte {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func quorumFactsSet(fields map[string]json.RawMessage, name string, raw string) {
	fields[name] = json.RawMessage(raw)
}

func quorumFactsMembers(t *testing.T, fields map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var members []map[string]json.RawMessage
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		t.Fatal(err)
	}
	return members
}

func quorumFactsSetMembers(t *testing.T, fields map[string]json.RawMessage, members []map[string]json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	fields["members"] = encoded
}

func quorumFactsCapacity(t *testing.T, count int) []byte {
	t.Helper()
	members := make([]string, count)
	for i := range members {
		// Independent literal tuples, rather than serializing the production DTO.
		members[i] = fmt.Sprintf(`{"id":"%d","address":"member-%03d.example:9010","role":"voter","key_fingerprint":"%064x"}`, i+1, i+1, i+1)
	}
	fields := quorumFactsFields(t)
	fields["members"] = json.RawMessage("[" + strings.Join(members, ",") + "]")
	return quorumFactsJSON(t, fields)
}

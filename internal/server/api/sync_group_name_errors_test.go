package api

import (
	"strings"
	"testing"
)

func TestSyncGroupNameErrorsWire(t *testing.T) {
	rig := newPullRig(t)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":" same "}`, 201,
		`{"id":2,"name":"same","node_requirements":{},"rate_limit_kib_s":0,"matched_nodes":0}`)
	duplicate := `{"code":"PULL_SETTING_INVALID","error":"name: a sync group with this name already exists","params":{"field":"name","detail":"a sync group with this name already exists","reason":"name_duplicate"}}`
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"same"}`, 400, duplicate)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"other","rate_limit_kib_s":42}`, 201,
		`{"id":3,"name":"other","node_requirements":{},"rate_limit_kib_s":42,"matched_nodes":0}`)
	rig.want("PUT", "/api/v1/pull/sync-groups/3", `{"name":"same","rate_limit_kib_s":99}`, 400, duplicate)
	groups, err := rig.st.SyncGroups()
	if err != nil || len(groups) != 3 || groups[2].Name != "other" || groups[2].RateLimitKiB != 42 {
		t.Fatalf("refused rename changed stored groups: %+v, %v", groups, err)
	}
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":" "}`, 400,
		`{"code":"PULL_SETTING_INVALID","error":"name: a sync group needs a name","params":{"field":"name","detail":"a sync group needs a name","reason":"name_required"}}`)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"`+strings.Repeat("g", 65)+`"}`, 400,
		`{"code":"PULL_SETTING_INVALID","error":"name: name is longer than 64 bytes","params":{"field":"name","detail":"name is longer than 64 bytes","reason":"max_bytes","max":64}}`)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"x","rate_limit_kib_s":-1}`, 400,
		`{"code":"PULL_SETTING_INVALID","error":"rate_limit: rate limit must not be negative","params":{"field":"rate_limit","detail":"rate limit must not be negative","reason":"nonnegative"}}`)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"x\n"}`, 400,
		`{"code":"PULL_SETTING_INVALID","error":"name: name must contain valid text without control characters","params":{"field":"name","detail":"name must contain valid text without control characters","reason":"invalid_text"}}`)
	if status, _ := rig.do(rig.admin, "PUT", "/api/v1/pull/sync-groups/9999", `{"name":"same"}`); status != 404 {
		t.Fatalf("missing group returned %d instead of 404", status)
	}
}

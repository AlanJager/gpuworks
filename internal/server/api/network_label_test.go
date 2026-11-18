package api

import (
	"net/http"
	"reflect"
	"testing"
)

func TestEditNetworkAttachmentLabel(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	const targets = "/api/v1/networks/1/attachments"
	const edit = targets + "/1/label"
	const newLabel = `{"label_key":" site ","label_value":" west "}`
	original := n.expect(n.admin, http.MethodPost, targets, `{"label_key":"pool","label_value":"a"}`, http.StatusCreated)["attachments"].([]any)[0].(map[string]any)
	n.expect(n.admin, http.MethodPost, targets, `{"label_key":"pool","label_value":"b"}`, http.StatusCreated)
	n.expect(n.member, http.MethodPut, edit, newLabel, http.StatusForbidden)
	n.expectCode(http.MethodPut, edit, `{"label_key":"bad key"}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPut, edit, `{"label_key":"pool","label_value":"b"}`, http.StatusConflict, ErrorNetworkAttachmentExists)
	n.expectCode(http.MethodPut, "/api/v1/networks/2/attachments/1/label", newLabel, http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	unchanged := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["attachments"].([]any)[0]
	if !reflect.DeepEqual(unchanged, original) {
		t.Fatal("rejected update changed the original attachment")
	}
	original["label_key"], original["label_value"] = "site", "west"
	for range 2 {
		result := n.expect(n.admin, http.MethodPut, edit, newLabel, http.StatusOK)["attachments"].([]any)
		if len(result) != 2 || !reflect.DeepEqual(result[0], original) {
			t.Fatalf("label update changed attachment identity, precedence or metadata: %v", result)
		}
	}
}

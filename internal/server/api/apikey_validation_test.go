package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAPIKeyCreationRejectsMissingNameWithoutCreatingKey(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"  \t\n"}`, `null`, `{"name":null}`} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"API_KEY_NAME_REQUIRED"`) {
			t.Errorf("body %s: got %d %s, want 400 API_KEY_NAME_REQUIRED", body, rec.Code, rec.Body)
		}
	}
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || list.Code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("invalid requests created keys: %d %s (err %v)", list.Code, list.Body, err)
	}
}

func TestAPIKeyCreationRejectsMalformedJSONWithoutCreatingKey(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, body := range []string{`{"name":"valid", "name":42}`, `{"name":true}`, `not json`, `{"name":"valid"`, ``} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: got %d, want 400", body, rec.Code)
		}
	}
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	if !strings.Contains(list.Body.String(), `"total":0`) {
		t.Fatalf("malformed requests created keys: %s", list.Body)
	}
}

func TestAPIKeyCreationReturnsTrimmedNameAndListsNoSecret(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"  inference client  "}`)
	var created map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated || len(created) != 2 || created["name"] != "inference client" || !strings.HasPrefix(created["key"], "gw-") {
		t.Fatalf("create response: status=%d name=%q fields=%d err=%v", rec.Code, created["name"], len(created), err)
	}
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"name":"inference client"`) || strings.Contains(list.Body.String(), created["key"]) {
		t.Fatal("list must preserve the chosen name without exposing the full key")
	}
}

func TestAPIKeyCreationBoundsNamesAndRequestBody(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, body := range []string{
		`{"name":"` + strings.Repeat("A", 200000) + `"}`,
		`{"name":"line\nbreak"}`, `{"name":"nul\u0000byte"}`,
		`{"name":"valid"} {"name":"second"}`,
		`{"name":"valid","padding":"` + strings.Repeat("A", 2<<20) + `"}`,
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("body length %d: got %d, want 400/413", len(body), rec.Code)
		}
	}
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	if !strings.Contains(list.Body.String(), `"total":0`) {
		t.Fatalf("invalid requests created keys (response bytes=%d)", list.Body.Len())
	}
	name := strings.Repeat("模", 256)
	body, _ := json.Marshal(map[string]string{"name": name})
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("Unicode name at limit: %d %s", rec.Code, rec.Body)
	}
}

// Revoking keeps the key listed with when it was revoked, so its past usage
// keeps a name; the key no longer authenticates and revoking it again is a
// no-op.
func TestAPIKeyRevokeKeepsTheKeyListed(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"client"}`)
	var created map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	prefix := created["key"][:12]
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	if strings.Contains(list.Body.String(), "revoked_at") {
		t.Fatalf("a live key lists revoked_at: %s", list.Body)
	}
	rec = rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+prefix, "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"revoked"}`+"\n" {
		t.Fatalf("revoke: %d %q", rec.Code, rec.Body)
	}
	if rig.st.VerifyAPIKey(created["key"]) {
		t.Fatal("a revoked key still authenticates")
	}
	list = rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list after revoke: %s", list.Body)
	}
	item := page.Items[0]
	revoked, _ := item["revoked_at"].(string)
	if item["name"] != "client" || item["prefix"] != prefix || !strings.HasSuffix(revoked, "Z") || item["key"] != nil || item["hash"] != nil {
		t.Fatalf("revoked item = %v", item)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+prefix, ""); rec.Code != http.StatusOK {
		t.Fatalf("second revoke: %d", rec.Code)
	}
	again := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	if !strings.Contains(again.Body.String(), `"revoked_at":"`+revoked+`"`) {
		t.Fatalf("a second revoke moved revoked_at: %s", again.Body)
	}
}

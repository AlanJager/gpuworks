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

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gorm.io/gorm"
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

func TestAPIKeyRevokeRejectsUnknownTargetsWithoutRevokingLiveKeys(t *testing.T) {
	for _, target := range []string{"unknown-prefix", "full-secret", "short-prefix"} {
		t.Run(target, func(t *testing.T) {
			rig := newTokenRig(t)
			t.Cleanup(func() { _ = rig.st.Close() })
			create := func(name string) string {
				t.Helper()
				rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"`+name+`"}`)
				var created map[string]string
				if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil || len(created["key"]) < 12 {
					t.Fatalf("create %s: status=%d", name, rec.Code)
				}
				return created["key"]
			}
			key, other := create("client"), create("other-client")
			unknown := "gw-000000000"
			for _, candidate := range []string{"gw-000000000", "gw-111111111", "gw-222222222"} {
				if candidate != key[:12] && candidate != other[:12] {
					unknown = candidate
					break
				}
			}
			if target == "full-secret" {
				unknown = key
			} else if target == "short-prefix" {
				unknown = key[:11]
			}
			// Populate the authentication cache before the rejected operation.
			if !rig.st.VerifyAPIKey(key) || !rig.st.VerifyAPIKey(other) {
				t.Fatal("created keys do not authenticate")
			}
			rec := rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+url.PathEscape(unknown), "")
			if rec.Code != http.StatusNotFound || rec.Body.String() != `{"code":"API_KEY_NOT_FOUND","error":"no API key has this prefix"}`+"\n" {
				t.Errorf("unknown target revoke: status=%d body=%s", rec.Code, rec.Body)
			}
			if !rig.st.VerifyAPIKey(key) || !rig.st.VerifyAPIKey(other) {
				t.Fatal("unknown-target revoke invalidated a live key")
			}
			keys := listedKeys(t, rig)
			if len(keys) != 2 || keys["client"]["prefix"] != key[:12] || keys["other-client"]["prefix"] != other[:12] || keys["client"]["revoked_at"] != nil || keys["other-client"]["revoked_at"] != nil {
				t.Fatalf("unknown-target revoke changed key metadata: %v", keys)
			}
		})
	}
}

func TestAPIKeyRevokeReportsWriteFailureWithoutRevokingKey(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	key, err := rig.st.CreateAPIKey("client", 0)
	if err != nil {
		t.Fatal(err)
	}
	const callback = "revoke_write_failure"
	if err := rig.st.DB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "api_keys" {
			tx.AddError(errors.New("injected API key write failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rig.st.DB().Callback().Update().Remove(callback) })
	rec := rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+key[:12], "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), `"status":"revoked"`) {
		t.Errorf("failed revoke: status=%d body=%s", rec.Code, rec.Body)
	}
	if !rig.st.VerifyAPIKey(key) || listedKeys(t, rig)["client"]["revoked_at"] != nil {
		t.Fatal("failed write revoked the key")
	}
}

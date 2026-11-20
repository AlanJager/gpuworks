package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

var settingsFenceKeys = []string{"scheduling.generation", "scheduling.owner", "scheduling.confirmation"}

func settingsFenceValues(st *store.Store) map[string]string {
	values := make(map[string]string, len(settingsFenceKeys))
	for _, key := range settingsFenceKeys {
		values[key] = st.GetSetting(store.SettingKey(key))
	}
	return values
}

// A controller that has taken scheduling leadership stores its fencing facts
// in the settings table. The full settings read must still answer, without
// them, and no client may overwrite them.
func TestSettingsRoutesHideAndProtectSchedulingFencingFacts(t *testing.T) {
	st, router, admin, viewer := settingsFencingRouter(t)
	if err := st.BeginSchedulingLeadership(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := settingsFenceValues(st)
	if before["scheduling.generation"] == "" || before["scheduling.owner"] == "" {
		t.Fatalf("leadership stored no fencing facts: %+v", before)
	}
	if err := st.SetSetting("scheduling.owner.note", `"operator text"`); err != nil {
		t.Fatal(err)
	}

	rec := settingsFencingRequest(t, router, http.MethodGet, "/api/v1/settings", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d %s, want 200 with fencing facts stored", rec.Code, rec.Body)
	}
	var listed map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	for _, key := range settingsFenceKeys {
		if _, ok := listed[key]; ok {
			t.Errorf("GET /api/v1/settings lists the fencing fact %q", key)
		}
	}
	if string(listed["scheduling.owner.note"]) != `"operator text"` {
		t.Errorf("operator key scheduling.owner.note = %s, want it listed", listed["scheduling.owner.note"])
	}

	for _, key := range settingsFenceKeys {
		path := "/api/v1/settings/" + key
		if rec := settingsFencingRequest(t, router, http.MethodPut, path, `"tampered"`, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous PUT %s = %d, want 401", path, rec.Code)
		}
		if rec := settingsFencingRequest(t, router, http.MethodPut, path, `"tampered"`, viewer); rec.Code != http.StatusForbidden {
			t.Fatalf("viewer PUT %s = %d, want 403", path, rec.Code)
		}
		for _, body := range []string{`"tampered"`, `{`} {
			rec := settingsFencingRequest(t, router, http.MethodPut, path, body, admin)
			var answer map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatalf("PUT %s body %s: %v", path, rec.Body, err)
			}
			if rec.Code != http.StatusForbidden || answer["error"] != "setting is reserved for internal scheduling" {
				t.Fatalf("admin PUT %s with %s = %d %s, want 403 reserved", path, body, rec.Code, rec.Body)
			}
		}
	}
	if after := settingsFenceValues(st); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused writes changed the fencing facts: %+v -> %+v", before, after)
	}
}

func TestSettingsRouteStillReportsCorruptPublicJSON(t *testing.T) {
	st, router, admin, _ := settingsFencingRouter(t)
	if err := st.SetSetting("scheduling.owner.note", "not-json"); err != nil {
		t.Fatal(err)
	}
	rec := settingsFencingRequest(t, router, http.MethodGet, "/api/v1/settings", "", admin)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || body["code"] != "SETTING_JSON_INVALID" || !reflect.DeepEqual(body["params"], map[string]any{"setting": "scheduling.owner.note"}) {
		t.Fatalf("corrupt public setting = %d %+v, want 500 SETTING_JSON_INVALID naming it", rec.Code, body)
	}
}

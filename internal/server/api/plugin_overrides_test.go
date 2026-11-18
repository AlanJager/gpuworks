package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

var overrideAt = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

func (f *fakePlugins) Overrides(context.Context) []dto.PluginOverride {
	_ = f.record("overrides")
	v := int64(3)
	return []dto.PluginOverride{{ID: 4, Owner: "acme_authority", Kind: "fleet_capacity", Entry: "max_cards", Generation: 2, Action: "loosen", Value: &v,
		ByUser: 1, At: overrideAt, Reason: "audit week"}}
}

func (f *fakePlugins) Override(_ context.Context, req dto.PluginOverrideRequest, by PluginAdministrator) (dto.PluginOverride, error) {
	value := "-"
	if req.Value != nil {
		value = fmt.Sprint(*req.Value)
	}
	if err := f.record(fmt.Sprintf("override %s %s %s %s %s %q %d %s", req.Owner, req.Kind, req.Entry, req.Action, value, req.Reason, by.UserID, by.Username)); err != nil {
		return dto.PluginOverride{}, err
	}
	return dto.PluginOverride{ID: 5, Owner: req.Owner, Kind: req.Kind, Entry: req.Entry, Generation: 1, Action: req.Action, Value: req.Value,
		ByUser: by.UserID, At: overrideAt, Reason: req.Reason}, nil
}

func (f *fakePlugins) RemoveOverride(_ context.Context, id int64, by PluginAdministrator) error {
	return f.record(fmt.Sprintf("remove_override %d %d %s", id, by.UserID, by.Username))
}

const holdOverrideBody = `{"owner":"acme_ops","kind":"node_holds","entry":"12","action":"delete","reason":"maintenance done"}`

// Who may call each override route: nobody unauthenticated, no member, an
// administrator's read or write token only for the list (a change refuses
// it as a platform token), and an administrator's session for everything.
func TestPluginOverrideRoutesAccess(t *testing.T) {
	rig := newPluginRig(t, &fakePlugins{})
	readToken, writeToken := rig.mint("read"), rig.mint("write")
	for _, tc := range []struct {
		method, path, body           string
		member, read, write, session int
	}{
		{"GET", "/api/v1/plugins/overrides", "", 403, 200, 200, 200},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, 403, 403, 403, 201},
		{"DELETE", "/api/v1/plugins/overrides/5", "", 403, 403, 403, 204},
	} {
		for _, who := range []struct {
			name, token string
			want        int
		}{{"anonymous", "", 401}, {"member", rig.member, tc.member}, {"read token", readToken, tc.read}, {"write token", writeToken, tc.write}, {"session", rig.session, tc.session}} {
			rec := rig.do(who.token, tc.method, tc.path, tc.body)
			if rec.Code != who.want {
				t.Errorf("%s %s as %s: %d %s, want %d", tc.method, tc.path, who.name, rec.Code, rec.Body, who.want)
			}
			if tc.method != "GET" && (who.name == "read token" || who.name == "write token") && !strings.Contains(rec.Body.String(), `"code":"PLATFORM_TOKEN_NOT_ALLOWED"`) {
				t.Errorf("%s %s as %s: %s; want PLATFORM_TOKEN_NOT_ALLOWED", tc.method, tc.path, who.name, rec.Body)
			}
		}
	}
	if rec := rig.do(rig.session, "PUT", "/api/v1/plugins/overrides/5", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT an override: %d", rec.Code)
	}
}

// The list, a create and a removal answer their literal wire shapes, and
// each change names the administrator by id and by name.
func TestPluginOverrideRoutesWire(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	rec := rig.do(rig.session, "GET", "/api/v1/plugins/overrides", "")
	want := `{"items":[{"id":4,"owner":"acme_authority","kind":"fleet_capacity","entry":"max_cards","generation":2,"action":"loosen","value":3,` +
		`"by_user":1,"at":"2026-11-18T09:30:00Z","reason":"audit week"}]}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("list: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/plugins/overrides", holdOverrideBody)
	want = `{"id":5,"owner":"acme_ops","kind":"node_holds","entry":"12","generation":1,"action":"delete","by_user":1,"at":"2026-11-18T09:30:00Z","reason":"maintenance done"}`
	if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("create: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/plugins/overrides",
		`{"owner":"acme_authority","kind":"fleet_capacity","entry":"max_cards","action":"loosen","value":16,"reason":"audit week"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"value":16`) {
		t.Fatalf("create a loosen: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, "DELETE", "/api/v1/plugins/overrides/5", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	got := strings.Join(plugins.recorded(), "|")
	if got != `overrides|override acme_ops node_holds 12 delete - "maintenance done" 1 admin|`+
		`override acme_authority fleet_capacity max_cards loosen 16 "audit week" 1 admin|remove_override 5 1 admin` {
		t.Fatalf("calls = %s", got)
	}
}

// A malformed request is refused before the plugin host is asked.
func TestPluginOverrideRoutesValidate(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	for _, tc := range []struct{ method, path, body, field string }{
		{"POST", "/api/v1/plugins/overrides", `{"owner":`, "body"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody + holdOverrideBody, "body"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"owner":"acme_ops"`, `"owner":""`, 1), "owner"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"kind":"node_holds",`, ``, 1), "kind"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"entry":"12"`, `"entry":" "`, 1), "entry"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"maintenance done"`, `""`, 1), "reason"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"maintenance done"`, `"`+strings.Repeat("x", 1025)+`"`, 1), "reason"},
		{"POST", "/api/v1/plugins/overrides", strings.Replace(holdOverrideBody, `"delete"`, `"tighten"`, 1), "action"},
		{"DELETE", "/api/v1/plugins/overrides/x", "", "oid"},
		{"DELETE", "/api/v1/plugins/overrides/0", "", "oid"},
	} {
		rec := rig.do(rig.session, tc.method, tc.path, tc.body)
		var body struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != "FIELD_INVALID" || body.Params["field"] != tc.field {
			t.Errorf("%s %s %.40s: %d %s; want 400 FIELD_INVALID on %s", tc.method, tc.path, tc.body, rec.Code, rec.Body, tc.field)
		}
	}
	if calls := plugins.recorded(); len(calls) != 0 {
		t.Fatalf("the plugin host was asked: %v", calls)
	}
}

// A refusal of the plugin host answers its status and keeps its code and
// params: no such override 404, every rule of an override 422.
func TestPluginOverrideRoutesMapRefusals(t *testing.T) {
	rejected := func(rule string) error {
		return messages.NewProductError(messages.ErrorPluginOverrideRejected, "no",
			map[string]any{"plugin": "acme_ops", "kind": "node_holds", "entry": "12", "rule": rule})
	}
	for _, tc := range []struct {
		method, path, body string
		err                error
		status             int
		param, value       string
	}{
		{"DELETE", "/api/v1/plugins/overrides/9", "", messages.NewProductError(messages.ErrorPluginOverrideNotFound, "none", map[string]any{"id": int64(9)}), 404, "id", "9"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, rejected("not_restrict"), 422, "rule", "not_restrict"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, rejected("no_entry"), 422, "rule", "no_entry"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, rejected("action"), 422, "rule", "action"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, rejected("not_looser"), 422, "rule", "not_looser"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, rejected("reason"), 422, "rule", "reason"},
		{"POST", "/api/v1/plugins/overrides", holdOverrideBody, fmt.Errorf("disk on fire"), 500, "", ""},
	} {
		rig := newPluginRig(t, &fakePlugins{err: tc.err})
		rec := rig.do(rig.session, tc.method, tc.path, tc.body)
		if rec.Code != tc.status {
			t.Fatalf("%v: %d %s, want %d", tc.err, rec.Code, rec.Body, tc.status)
		}
		if tc.param == "" {
			continue
		}
		var body struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		var pe *messages.ProductError
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if !errors.As(tc.err, &pe) || body.Code != pe.Code() || fmt.Sprint(body.Params[tc.param]) != tc.value {
			t.Fatalf("%v: body %s", tc.err, rec.Body)
		}
	}
}

// Without a plugin host the override routes exist and say so; the
// assistant reads none of them.
func TestPluginOverrideRoutesWithoutAHostAndForTheAssistant(t *testing.T) {
	rig := newPluginRig(t, nil)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/plugins/overrides", ""}, {"POST", "/api/v1/plugins/overrides", holdOverrideBody}, {"DELETE", "/api/v1/plugins/overrides/1", ""},
	} {
		rec := rig.do(rig.session, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), messages.ErrorPluginsUnavailable) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	withHost := newPluginRig(t, &fakePlugins{})
	req := httptest.NewRequest("GET", "/api/v1/plugins/overrides", nil)
	req.Header.Set("Authorization", "Bearer "+withHost.session)
	req = req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true))
	rec := httptest.NewRecorder()
	withHost.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || len(withHost.plugins.recorded()) != 0 {
		t.Fatalf("assistant read: %d %s", rec.Code, rec.Body)
	}
}

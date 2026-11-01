package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func scopeRigDeployment(t *testing.T, st *store.Store, name string) uint {
	t.Helper()
	dep, err := st.CreateDeployment(&store.Deployment{Name: name, ServedName: name + "-served"})
	if err != nil {
		t.Fatal(err)
	}
	return dep.ID
}

// listedKeys is GET /api/v1/apikeys decoded loosely, by key name.
func listedKeys(t *testing.T, rig *tokenRig) map[string]map[string]any {
	t.Helper()
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	out := map[string]map[string]any{}
	for _, k := range list.Items {
		out[k["name"].(string)] = k
	}
	return out
}

func TestAPIKeyCreateWithScopeWire(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	a := scopeRigDeployment(t, rig.st, "qwen")

	// The UI's two shapes, plus the body of a client that predates scopes.
	for _, body := range []string{
		`{"name":"legacy"}`,
		`{"name":"all","all_deployments":true,"deployment_ids":[]}`,
		fmt.Sprintf(`{"name":"scoped","all_deployments":false,"deployment_ids":[%d,%d]}`, a, a),
		fmt.Sprintf(`{"name":"implicit","deployment_ids":[%d]}`, a),
	} {
		if rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", body); rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	keys := listedKeys(t, rig)
	for _, name := range []string{"legacy", "all"} {
		if keys[name]["all_deployments"] != true || !reflect.DeepEqual(keys[name]["deployments"], []any{}) {
			t.Fatalf("%s = %v", name, keys[name])
		}
	}
	want := []any{map[string]any{"id": float64(a), "name": "qwen", "served_name": "qwen-served"}}
	for _, name := range []string{"scoped", "implicit"} {
		if keys[name]["all_deployments"] != false || !reflect.DeepEqual(keys[name]["deployments"], want) {
			t.Fatalf("%s = %v", name, keys[name])
		}
	}
}

func TestAPIKeyCreateRefusesBadScopesWithoutCreatingAKey(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	a := scopeRigDeployment(t, rig.st, "qwen")
	for _, tc := range []struct{ body, want string }{
		{fmt.Sprintf(`{"name":"k","all_deployments":true,"deployment_ids":[%d]}`, a), `"code":"API_KEY_SCOPE_INVALID"`},
		{`{"name":"k","all_deployments":false}`, `"code":"API_KEY_SCOPE_INVALID"`},
		{`{"name":"k","all_deployments":false,"deployment_ids":[]}`, `"code":"API_KEY_SCOPE_INVALID"`},
		{`{"name":"k","deployment_ids":[0]}`, `"code":"API_KEY_SCOPE_INVALID"`},
		{fmt.Sprintf(`{"name":"k","deployment_ids":[%d,999]}`, a), `"code":"API_KEY_DEPLOYMENT_UNKNOWN","error":"unknown deployments [999]","params":{"ids":[999]}`},
		{`{"name":"k","deployment_ids":"1"}`, `"error"`},
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want 400 with %s", tc.body, rec.Code, rec.Body, tc.want)
		}
	}
	if keys := listedKeys(t, rig); len(keys) != 0 {
		t.Fatalf("refused creates left keys: %v", keys)
	}
}

func TestAPIKeySetScopeWire(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	a, b := scopeRigDeployment(t, rig.st, "qwen"), scopeRigDeployment(t, rig.st, "llama")
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"k"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	prefix := listedKeys(t, rig)["k"]["prefix"].(string)
	path := "/api/v1/apikeys/" + prefix + "/scope"

	rec := rig.do(rig.session, http.MethodPut, path, fmt.Sprintf(`{"all_deployments":false,"deployment_ids":[%d,%d]}`, b, a))
	if rec.Code != http.StatusOK {
		t.Fatalf("narrow: %d %s", rec.Code, rec.Body)
	}
	var item map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"id": float64(a), "name": "qwen", "served_name": "qwen-served"},
		map[string]any{"id": float64(b), "name": "llama", "served_name": "llama-served"},
	}
	if item["prefix"] != prefix || item["all_deployments"] != false || !reflect.DeepEqual(item["deployments"], want) || item["key"] != nil {
		t.Fatalf("narrowed item = %v", item)
	}

	for _, tc := range []struct {
		body   string
		status int
		want   string
	}{
		{fmt.Sprintf(`{"deployment_ids":[%d]}`, a), http.StatusBadRequest, `"code":"API_KEY_SCOPE_INVALID"`},
		{`{"all_deployments":false,"deployment_ids":[]}`, http.StatusBadRequest, `"code":"API_KEY_SCOPE_INVALID"`},
		{`{"all_deployments":false,"deployment_ids":[424242]}`, http.StatusBadRequest, `"code":"API_KEY_DEPLOYMENT_UNKNOWN"`},
		{`not json`, http.StatusBadRequest, `"error"`},
	} {
		if rec := rig.do(rig.session, http.MethodPut, path, tc.body); rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	// Refused edits kept the narrowed scope.
	if got := listedKeys(t, rig)["k"]["deployments"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("after refused edits: %v", got)
	}

	rec = rig.do(rig.session, http.MethodPut, path, `{"all_deployments":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"all_deployments":true,"deployments":[]`) {
		t.Fatalf("widen: %d %s", rec.Code, rec.Body)
	}

	read := rig.mint("read")
	if rec := rig.do(read, http.MethodPut, path, `{"all_deployments":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/apikeys/gw-unknown0/scope", `{"all_deployments":true}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"API_KEY_NOT_FOUND"`) {
		t.Fatalf("unknown prefix: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+prefix, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, path, `{"all_deployments":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked key: %d %s", rec.Code, rec.Body)
	}
}

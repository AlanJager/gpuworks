package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

type scheduleRig struct {
	*tokenRig
	plain, multi *store.Deployment
}

func newScheduleRig(t *testing.T) *scheduleRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	create := func(dep *store.Deployment) *store.Deployment {
		dep.State = store.DeployActive
		created, err := rig.st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	return &scheduleRig{
		tokenRig: rig,
		plain:    create(&store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm", Replicas: 1}),
		multi:    create(&store.Deployment{Name: "multi", ServedName: "multi", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1}),
	}
}

func schedPath(dep *store.Deployment, tail string) string {
	return fmt.Sprintf("/api/v1/deployments/%d/%s", dep.ID, tail)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d body %q is not a JSON object: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// deploymentWire reads the deployment as the API serves it.
func (r *scheduleRig) deploymentWire(t *testing.T, dep *store.Deployment) map[string]any {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", dep.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deployment = %d %s", rec.Code, rec.Body)
	}
	return decodeBody(t, rec)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestNodeReplacementRouteAuthAndTarget(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.plain, "node-replacement")
	const body = `{"after_sec":120}`

	if rec := rig.do("", http.MethodPut, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s, want 401", rec.Code, rec.Body)
	}
	read := rig.mint("read")
	if rec := rig.do(read, http.MethodPut, path, body); rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
		t.Fatalf("read token = %d %s, want 403 PLATFORM_TOKEN_READ_ONLY", rec.Code, rec.Body)
	}
	if dep, _ := rig.st.GetDeployment(rig.plain.ID); dep.NodeReplaceAfterSec != 600 {
		t.Fatalf("a refused request changed the setting to %d", dep.NodeReplaceAfterSec)
	}
	if rec := rig.do(rig.mint("write"), http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/deployments/9999/node-replacement", body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d %s, want 404", rec.Code, rec.Body)
	}
	// Routing answers before the scope check, for either token.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		for _, token := range []string{rig.session, read} {
			if rec := rig.do(token, method, path, "{}"); rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s = %d %s, want 405", method, rec.Code, rec.Body)
			}
		}
	}
}

func TestNodeReplacementSetting(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.plain, "node-replacement")

	if got := rig.deploymentWire(t, rig.plain)["node_replacement"]; jsonOf(t, got) != `{"after_sec":600}` {
		t.Fatalf("a new deployment's node_replacement = %s, want 600 s", jsonOf(t, got))
	}

	// 0 is "never move a replica", and it must survive the column's default of 600.
	for _, tc := range []struct {
		body string
		want int
	}{{`{"after_sec":0}`, 0}, {`{"after_sec":600}`, 600}, {`{"after_sec":60}`, 60}, {`{"after_sec":604800}`, 604800}, {`{"after_sec":0}`, 0}} {
		rec := rig.do(rig.session, http.MethodPut, path, tc.body)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Fatalf("PUT %s = %d %s", tc.body, rec.Code, rec.Body)
		}
		if got := rig.deploymentWire(t, rig.plain)["node_replacement"]; jsonOf(t, got) != tc.body {
			t.Fatalf("after PUT %s, node_replacement = %s", tc.body, jsonOf(t, got))
		}
		if dep, _ := rig.st.GetDeployment(rig.plain.ID); dep.NodeReplaceAfterSec != tc.want {
			t.Fatalf("stored = %d, want %d", dep.NodeReplaceAfterSec, tc.want)
		}
	}
	if rec := rig.do(rig.session, http.MethodPut, path, `{"after_sec":900}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}

	for _, body := range []string{`{"after_sec":30}`, `{"after_sec":59}`, `{"after_sec":-60}`, `{"after_sec":604801}`} {
		rec := rig.do(rig.session, http.MethodPut, path, body)
		resp := decodeBody(t, rec)
		params, _ := resp["params"].(map[string]any)
		if rec.Code != http.StatusBadRequest || resp["code"] != "NODE_REPLACEMENT_INVALID" || params["minimum"] != float64(60) || params["maximum"] != float64(604800) {
			t.Fatalf("PUT %s = %d %s, want 400 NODE_REPLACEMENT_INVALID with bounds", body, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"after_sec":600,"level":1}`, `{"after_sec":"600"}`, `{"after_sec":`, ``} {
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if dep, _ := rig.st.GetDeployment(rig.plain.ID); dep.NodeReplaceAfterSec != 900 {
		t.Fatalf("a refused request changed the setting to %d", dep.NodeReplaceAfterSec)
	}
	// Node replacement does not need sleep mode.
	if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.multi, "node-replacement"), `{"after_sec":120}`); rec.Code != http.StatusOK {
		t.Fatalf("multi-node = %d %s", rec.Code, rec.Body)
	}
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Exhausting retries retains all evidence, but only the newest failure is
// current. The verdict is identical across detail, list and deployment reads,
// even when a page contains only one historical instance.
func TestPreviousAttemptWireAfterRetryExhaustion(t *testing.T) {
	f := newListFixture(t, 0)
	node, err := f.st.NodeByUUID("list-a")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "exhausted", ServedName: "exhausted", State: store.DeployActive, NodeID: node.ID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "healthy-sibling", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		inst := &store.Instance{UUID: fmt.Sprintf("exhausted-%d", i), DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstFailed, Message: "health check timed out"}
		if _, err := f.st.CreateInstance(inst); err != nil {
			t.Fatal(err)
		}
		if err := f.st.DB().Model(inst).Update("created_at", time.Date(2026, 11, 18, 0, 0, i, 0, time.UTC)).Error; err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			f.st.AddInstanceEventCode(inst, store.EventKindSchedule, store.EventUnitRetryExhausted, map[string]any{"attempts": 3})
		}
	}
	check := func(raw []byte, want bool) {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		value, present := object["previous_attempt"]
		if want && string(value) != "true" || !want && present {
			t.Fatalf("previous_attempt=%s present=%v want %v: %s", value, present, want, raw)
		}
		if want {
			var round struct {
				Previous bool `json:"previous_attempt"`
			}
			if err := json.Unmarshal(raw, &round); err != nil || !round.Previous {
				t.Fatalf("round-trip: %+v %v", round, err)
			}
		}
	}
	for i := 0; i < 3; i++ {
		uuid := fmt.Sprintf("exhausted-%d", i)
		rec := instanceRequest(f.handler.getInstance, uuid, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET instance: %d %s", rec.Code, rec.Body.String())
		}
		check(rec.Body.Bytes(), i < 2)
	}
	for _, path := range []string{"/api/v1/instances?q=exhausted", "/api/v1/instances?page=1&page_size=1&q=exhausted-0"} {
		raw := f.get(t, f.handler.listInstances, path)
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) == 0 {
			t.Fatalf("list=%s err=%v", raw, err)
		}
		for _, item := range list.Items {
			check(item, strings.Contains(string(item), `"uuid":"exhausted-0"`) || strings.Contains(string(item), `"uuid":"exhausted-1"`))
		}
	}
	rec := sleepCallByID(f.handler.getDeployment, dep.ID)
	var detail struct {
		Instances []json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Instances) != 4 {
		t.Fatalf("deployment=%s err=%v", rec.Body.String(), err)
	}
	for _, item := range detail.Instances {
		check(item, strings.Contains(string(item), `"uuid":"exhausted-0"`) || strings.Contains(string(item), `"uuid":"exhausted-1"`))
	}
	// Once the final missing replica is filled, the newest failure is historical too.
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "replacement", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	check(instanceRequest(f.handler.getInstance, "exhausted-2", "").Body.Bytes(), true)
	// A standalone JSON response must not smuggle the derived verdict into storage.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instances?q=healthy-sibling", nil)
	out := httptest.NewRecorder()
	f.handler.listInstances(out, req)
	if strings.Contains(out.Body.String(), `"previous_attempt"`) {
		t.Fatalf("healthy sibling: %s", out.Body.String())
	}
}

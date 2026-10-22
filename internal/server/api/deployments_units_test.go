package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A unit that spent its retry budget shows it in the deployment response,
// and a retry gives it back without touching the other units.
func TestRetryUnitGivesTheBudgetBack(t *testing.T) {
	h, st, created := createPDForOps(t)
	dep := mustDeployment(t, st, created.ID)
	nodeA, err := st.NodeByUUID("pd-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "healthy", DeploymentID: dep.ID, NodeID: nodeA.ID, GPUIndexes: "[0]", State: proto.InstRunning, Role: "prefill"}); err != nil {
		t.Fatal(err)
	}
	var newest *store.Instance
	for i := 0; i < 3; i++ {
		row := &store.Instance{UUID: fmt.Sprintf("attempt-%d", i), DeploymentID: dep.ID, NodeID: nodeA.ID, GPUIndexes: "[1]", State: proto.InstFailed, Role: "prefill", Message: fmt.Sprintf("image pull failed %d", i)}
		if _, err := st.CreateInstance(row); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", row.UUID).Update("created_at", time.Now().Add(time.Duration(i)*time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		newest = row
	}
	st.AddInstanceEventCode(newest, store.EventKindSchedule, store.EventUnitRetryExhausted, map[string]any{"attempts": 3})

	view := h.deploymentDTO(dep, true)
	unit := view.PD.Roles[1].Units[1]
	if view.PD.Roles[1].Role != "prefill" || unit.FailedAttempts != 3 || !unit.RetryExhausted || unit.LastFailure != "image pull failed 2" {
		t.Fatalf("exhausted unit = %+v", unit)
	}
	if other := view.PD.Roles[1].Units[0]; other.FailedAttempts != 0 || other.RetryExhausted {
		t.Fatalf("healthy unit = %+v", other)
	}
	raw, _ := json.Marshal(unit)
	for _, key := range []string{`"failed_attempts":3`, `"retry_exhausted":true`, `"last_failure":"image pull failed 2"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("wire unit %s lacks %s", raw, key)
		}
	}

	rec := pdOpsRequest(h.retryUnit, http.MethodPost, dep.ID, `{"role":"prefill","index":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		row, _ := st.InstanceByUUID(fmt.Sprintf("attempt-%d", i))
		if row.State != proto.InstRemoving {
			t.Fatalf("attempt %d = %s, want removing", i, row.State)
		}
	}
	if row, _ := st.InstanceByUUID("healthy"); row.State != proto.InstRunning {
		t.Fatalf("another unit was touched: %s", row.State)
	}
	if event, found, _ := st.LatestScheduleEvent(dep.ID); !found || event.Code != store.EventUnitRetried || !strings.Contains(event.Data, `"attempts":3`) {
		t.Fatalf("retry event = %+v", event)
	}
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if got := out.PD.Roles[1].Units[1]; got.FailedAttempts != 0 || got.RetryExhausted {
		t.Fatalf("unit after retry = %+v", got)
	}
}

func TestRetryUnitRefusals(t *testing.T) {
	h, st, created := createPDForOps(t)
	dep := mustDeployment(t, st, created.ID)
	cases := []struct {
		name, body string
		id         uint
		status     int
		code       string
	}{
		{name: "unknown deployment", id: 9999, body: `{"role":"prefill","index":0}`, status: http.StatusNotFound},
		{name: "unreadable body", id: dep.ID, body: `{"role":`, status: http.StatusBadRequest},
		{name: "unknown field", id: dep.ID, body: `{"role":"prefill","index":0,"force":true}`, status: http.StatusBadRequest},
		{name: "unknown unit", id: dep.ID, body: `{"role":"prefill","index":7}`, status: http.StatusNotFound, code: ErrorUnitUnknown},
		{name: "unknown role", id: dep.ID, body: `{"role":"router","index":0}`, status: http.StatusNotFound, code: ErrorUnitUnknown},
		{name: "nothing failed", id: dep.ID, body: `{"role":"decode","index":0}`, status: http.StatusConflict, code: ErrorUnitNotFailed},
	}
	for _, tc := range cases {
		rec := pdOpsRequest(h.retryUnit, http.MethodPost, tc.id, tc.body)
		if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), tc.code)) {
			t.Fatalf("%s = %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
	if err := st.SetDeploymentState(dep.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if rec := pdOpsRequest(h.retryUnit, http.MethodPost, dep.ID, `{"role":"decode","index":0}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorUnitRetryNotActive) {
		t.Fatalf("stopped = %d %s", rec.Code, rec.Body.String())
	}
}

// An ordinary deployment's replicas are units too: the role is empty.
func TestRetryUnitOfAnOrdinaryDeployment(t *testing.T) {
	h, st, created := createPDForOps(t)
	node := mustDeployment(t, st, created.ID).NodeID
	plain, err := st.CreateDeployment(&store.Deployment{Name: "plain", ServedName: "plain", State: "active", NodeID: node, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "plain-failed", DeploymentID: plain.ID, NodeID: node, State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}
	if rec := pdOpsRequest(h.retryUnit, http.MethodPost, plain.ID, `{"role":"","index":0}`); rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	if row, _ := st.InstanceByUUID("plain-failed"); row.State != proto.InstRemoving {
		t.Fatalf("failed replica = %s", row.State)
	}
}

// An ordinary deployment whose replica gave up names that replica in its
// response, so the drawer can offer the retry; a retry of the filled replica
// is refused, a retry of the named one gives the shared budget back.
func TestOrdinaryDeploymentReportsTheReplicaThatGaveUp(t *testing.T) {
	h, st, created := createPDForOps(t)
	pd := mustDeployment(t, st, created.ID)
	plain, err := st.CreateDeployment(&store.Deployment{Name: "plain", ServedName: "plain", State: "active", NodeID: pd.NodeID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "plain-running", DeploymentID: plain.ID, NodeID: plain.NodeID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	var newest *store.Instance
	for i := 0; i < 3; i++ {
		row := &store.Instance{UUID: fmt.Sprintf("plain-attempt-%d", i), DeploymentID: plain.ID, NodeID: plain.NodeID, State: proto.InstFailed, Message: fmt.Sprintf("engine exited %d", i)}
		if _, err := st.CreateInstance(row); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", row.UUID).Update("created_at", time.Now().Add(time.Duration(i)*time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		newest = row
	}

	// Failed attempts alone are not a give-up: the controller is still
	// rebuilding within its budget.
	if view := h.deploymentDTO(mustDeployment(t, st, plain.ID), true); view.RetryExhausted != nil {
		t.Fatalf("attempts within budget reported as exhausted: %+v", view.RetryExhausted)
	}
	st.AddInstanceEventCode(newest, store.EventKindSchedule, store.EventUnitRetryExhausted, map[string]any{"attempts": 3})
	view := h.deploymentDTO(mustDeployment(t, st, plain.ID), true)
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `"retry_exhausted":{"index":1,"failed_attempts":3,"last_failure":"engine exited 2"}`) {
		t.Fatalf("wire deployment lacks the exhausted replica: %s", raw)
	}
	// A PD deployment reports give-ups per unit, never at the top.
	if pdRaw, _ := json.Marshal(h.deploymentDTO(pd, true)); strings.Contains(string(pdRaw), `"retry_exhausted":{`) {
		t.Fatalf("PD deployment carries a top-level retry: %s", pdRaw)
	}

	if rec := pdOpsRequest(h.retryUnit, http.MethodPost, plain.ID, `{"role":"","index":0}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorUnitNotFailed) {
		t.Fatalf("retry of the filled replica = %d %s", rec.Code, rec.Body.String())
	}
	rec := pdOpsRequest(h.retryUnit, http.MethodPost, plain.ID, `{"role":"","index":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.RetryExhausted != nil {
		t.Fatalf("retried deployment still exhausted: %+v", out.RetryExhausted)
	}
	for i := 0; i < 3; i++ {
		if row, _ := st.InstanceByUUID(fmt.Sprintf("plain-attempt-%d", i)); row.State != proto.InstRemoving {
			t.Fatalf("attempt %d = %s, want removing", i, row.State)
		}
	}
	if row, _ := st.InstanceByUUID("plain-running"); row.State != proto.InstRunning {
		t.Fatalf("the running replica was touched: %s", row.State)
	}
}

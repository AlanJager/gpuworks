package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func setModelOperations(t *testing.T, st *store.Store, modelID uint, ops string) {
	t.Helper()
	if err := st.DB().Model(&store.Model{}).Where("id = ?", modelID).Update("operations", ops).Error; err != nil {
		t.Fatal(err)
	}
}

// A new deployment's health check is on when the model serves chat or
// embeddings and the request says nothing, as the request says otherwise;
// a threshold out of range is refused, and one the model cannot be checked
// with is refused when asked for explicitly.
func TestCreateDeploymentHealthCheck(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	h.d.Monitor = &fakeMonitorService{}
	creates := 0
	create := func(extra string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		// Each create states its own served name: a second deployment may not
		// answer under the name the first holds.
		creates++
		body := fmt.Sprintf(`{"name":"checked","served_name":"checked-%d","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}%s}`, creates, modelID, extra)
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body is not JSON: %s", rec.Body)
		}
		return rec.Code, out
	}
	stored := func(out map[string]any) *store.Deployment {
		t.Helper()
		row, err := st.GetDeployment(uint(out["id"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	setModelOperations(t, st, modelID, `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`)
	status, out := create("")
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v", status, out)
	}
	if row := stored(out); !row.HealthCheck || row.HealthCheckThresholdSec != 60 {
		t.Fatalf("a chat deployment saying nothing: %+v", row)
	}
	health := out["health"].(map[string]any)
	if health["status"] != "pending" || health["threshold_sec"] != float64(60) {
		t.Fatalf("health = %v", health)
	}

	status, out = create(`,"health_check":false,"health_check_threshold_sec":15`)
	if row := stored(out); status != http.StatusCreated || row.HealthCheck || row.HealthCheckThresholdSec != 15 {
		t.Fatalf("turned off: %d %+v", status, row)
	}
	if out["health"].(map[string]any)["status"] != "off" {
		t.Fatalf("health = %v", out["health"])
	}

	for _, bad := range []int{-1, 601} {
		status, out = create(fmt.Sprintf(`,"health_check_threshold_sec":%d`, bad))
		if status != http.StatusBadRequest || out["code"] != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(out["params"]), "between 1 and 600") {
			t.Fatalf("threshold %d: %d %v", bad, status, out)
		}
	}

	setModelOperations(t, st, modelID, `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`)
	status, out = create("")
	if row := stored(out); status != http.StatusCreated || row.HealthCheck {
		t.Fatalf("a rerank deployment saying nothing: %d %+v", status, row)
	}
	status, out = create(`,"health_check":true`)
	if status != http.StatusBadRequest || out["code"] != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(out["params"]), "health_check") {
		t.Fatalf("an uncheckable model asked to be checked: %d %v", status, out)
	}
}

// PUT /deployments/{id}/health-check turns the check on or off and sets its
// threshold; the deployment it answers carries the health the monitor
// reports, under the wire keys the UI reads.
func TestSetHealthCheckRoute(t *testing.T) {
	signer, err := auth.NewTokenSigner("health-check-route-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:health_check_route?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	latency := int64(180)
	mon := &fakeMonitorService{health: dto.DeploymentHealth{
		Status: "ok", RecentMeanMS: &latency, RecentOK: 1,
		Last:   &dto.HealthProbe{AtMS: 1000, Status: "ok", LatencyMS: 180, HTTPStatus: 200, InstanceUUID: "i-1"},
		Recent: []dto.HealthProbe{{AtMS: 1000, Status: "ok", LatencyMS: 180, HTTPStatus: 200, InstanceUUID: "i-1"}},
	}}
	router := Router(Deps{AuthSigner: signer, Store: st, Monitor: mon})
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	put := func(path, body string, withAuth bool) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	chat, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/chat", Revision: "main", Name: "chat", Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	if err != nil {
		t.Fatal(err)
	}
	rerank, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/rerank", Revision: "main", Name: "rerank", Operations: `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "checked", ServedName: "checked", ModelID: chat.ID, State: store.DeployActive, HealthCheckThresholdSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10) + "/health-check"

	if status, _ := put(path, `{"enabled":true}`, false); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", status)
	}
	if status, _ := put("/api/v1/deployments/9999/health-check", `{"enabled":true}`, true); status != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", status)
	}
	if status, _ := put(path, `{"enabled":`, true); status != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", status)
	}
	if status, out := put(path, `{"enabled":true,"threshold_sec":601}`, true); status != http.StatusBadRequest || out["code"] != ErrorFieldInvalid {
		t.Fatalf("threshold out of range = %d %v", status, out)
	}

	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"enabled":true,"threshold_sec":20}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn on = %d %s", rec.Code, rec.Body)
	}
	var wire struct {
		Health json.RawMessage `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	const want = `{"status":"ok","threshold_sec":20,"last":{"at_ms":1000,"status":"ok","latency_ms":180,"http_status":200,"instance_uuid":"i-1"},"recent_mean_ms":180,"recent_ok":1,"recent":[{"at_ms":1000,"status":"ok","latency_ms":180,"http_status":200,"instance_uuid":"i-1"}]}`
	if string(wire.Health) != want {
		t.Fatalf("health =\n%s\nwant\n%s", wire.Health, want)
	}
	if row, _ := st.GetDeployment(dep.ID); !row.HealthCheck || row.HealthCheckThresholdSec != 20 {
		t.Fatalf("stored = %+v", row)
	}

	// Off keeps the threshold when none is given.
	if status, out := put(path, `{"enabled":false}`, true); status != http.StatusOK || out["health"].(map[string]any)["status"] != "off" || out["health"].(map[string]any)["threshold_sec"] != float64(20) {
		t.Fatalf("turn off = %d %v", status, out)
	}
	if row, _ := st.GetDeployment(dep.ID); row.HealthCheck || row.HealthCheckThresholdSec != 20 {
		t.Fatalf("stored = %+v", row)
	}

	other, err := st.CreateDeployment(&store.Deployment{Name: "ranker", ServedName: "ranker", ModelID: rerank.ID, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	otherPath := "/api/v1/deployments/" + strconv.FormatUint(uint64(other.ID), 10) + "/health-check"
	if status, out := put(otherPath, `{"enabled":true}`, true); status != http.StatusBadRequest || out["code"] != ErrorFieldInvalid {
		t.Fatalf("uncheckable = %d %v", status, out)
	}
	if status, _ := put(otherPath, `{"enabled":false}`, true); status != http.StatusOK {
		t.Fatalf("turning off an uncheckable deployment = %d", status)
	}
}

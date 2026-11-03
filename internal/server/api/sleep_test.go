package api

import (
	"encoding/json"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The policy package produces the capability and admission codes. API-only
// parse errors remain in this inventory so client and locale checks cover them.
func TestSleepCodesMatchTheirProducer(t *testing.T) {
	for api, producer := range map[string]string{
		ErrorSleepNotEnabled:           sleep.CodeNotEnabled,
		ErrorSleepEngineUnsupported:    sleep.CodeEngineUnsupported,
		ErrorSleepInvalidLevel:         "SLEEP_INVALID_LEVEL",
		ErrorSleepInvalidMode:          "SLEEP_INVALID_MODE",
		ErrorWakeInsufficientGPUMemory: "WAKE_INSUFFICIENT_GPU_MEMORY",
	} {
		if api != producer {
			t.Fatalf("api code %q, sleep code %q", api, producer)
		}
	}
}

type sleepFixture struct {
	st   *store.Store
	node *store.Node
	h    *apiHandlers
}

func newSleepFixture(t *testing.T) sleepFixture {
	t.Helper()
	st, err := store.Open("file:api_sleep_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node", store.HashToken("token"), "sleep-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, UUID: "gpu-0", NodeID: node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 24 << 30}}
	if err := st.DB().Create(&node.GPUs[0]).Error; err != nil {
		t.Fatal(err)
	}
	return sleepFixture{st: st, node: node, h: &apiHandlers{d: Deps{Store: st}}}
}

func (f sleepFixture) deployment(t *testing.T, dep *store.Deployment) *store.Deployment {
	t.Helper()
	dep.ModelID = 1
	dep.NodeID = f.node.ID
	dep.State = store.DeployActive
	created, err := f.st.CreateDeployment(dep)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (f sleepFixture) instance(t *testing.T, dep *store.Deployment, uuid string, sleepLevel int) *store.Instance {
	t.Helper()
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: dep.Sleepable, DesiredSleep: dep.DesiredSleep, APIPort: 8000})
	inst, err := f.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstRunning, GPUIndexes: "[0]", SleepLevel: sleepLevel, APIPort: 8000, DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func (f sleepFixture) call(t *testing.T, handler http.HandlerFunc, path string, id uint) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": strconv.FormatUint(uint64(id), 10)}))
	rec := httptest.NewRecorder()
	handler(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d body %q is not JSON: %v", rec.Code, rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestSleepDeploymentRefusesWhatCannotSleep(t *testing.T) {
	f := newSleepFixture(t)
	notEnabled := f.deployment(t, &store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm"})
	sglang := f.deployment(t, &store.Deployment{Name: "sg", ServedName: "sg", Engine: "sglang", Sleepable: true})
	for _, tc := range []struct {
		name string
		id   uint
		path string
		want int
		code string
	}{
		{name: "missing deployment", id: 9999, path: "/sleep", want: http.StatusNotFound},
		{name: "not enabled", id: notEnabled.ID, path: "/sleep", want: http.StatusBadRequest, code: "SLEEP_NOT_ENABLED"},
		{name: "engine unsupported", id: sglang.ID, path: "/sleep", want: http.StatusBadRequest, code: "SLEEP_ENGINE_UNSUPPORTED"},
		{name: "wake not enabled", id: notEnabled.ID, path: "/wake", want: http.StatusBadRequest, code: "SLEEP_NOT_ENABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := f.h.sleepDeployment
			if tc.path == "/wake" {
				handler = f.h.wakeDeployment
			}
			status, body := f.call(t, handler, "/api/v1/deployments/x"+tc.path, tc.id)
			if status != tc.want {
				t.Fatalf("status = %d body %v, want %d", status, body, tc.want)
			}
			if tc.code != "" && body["code"] != tc.code {
				t.Fatalf("code = %v, want %s (body %v)", body["code"], tc.code, body)
			}
		})
	}
}

func TestSleepDeploymentValidatesLevelAndModeAndStampsTheOrder(t *testing.T) {
	f := newSleepFixture(t)
	dep := f.deployment(t, &store.Deployment{Name: "s", ServedName: "s", Engine: "vllm", Sleepable: true})
	f.instance(t, dep, "inst-1", 0)
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "inst-stopped", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstStopped, DesiredStateJSON: `{"uuid":"inst-stopped"}`}); err != nil {
		t.Fatal(err)
	}

	status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=3", dep.ID)
	if status != http.StatusBadRequest || body["code"] != "SLEEP_INVALID_LEVEL" || body["params"].(map[string]any)["level"] != "3" {
		t.Fatalf("level=3: %d %v", status, body)
	}
	status, body = f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?mode=keep", dep.ID)
	if status != http.StatusBadRequest || body["code"] != "SLEEP_INVALID_MODE" || body["params"].(map[string]any)["mode"] != "keep" {
		t.Fatalf("mode=keep: %d %v", status, body)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("a refused order changed the desired level to %d", got.DesiredSleep)
	}

	status, body = f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=2&mode=abort", dep.ID)
	if status != http.StatusOK {
		t.Fatalf("sleep: %d %v", status, body)
	}
	if raw, _ := json.Marshal(body); string(raw) != `{"level":2,"mode":"abort","status":"sleeping","wake_on_request":false}` {
		t.Fatalf("sleep body = %s", raw)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 2 {
		t.Fatalf("desired level = %d, want 2", got.DesiredSleep)
	}
	inst, _ := f.st.InstanceByUUID("inst-1")
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SleepMode != "abort" || spec.DesiredSleep != 2 {
		t.Fatalf("live instance spec = %+v, want the order's level and mode", spec)
	}
	stopped, _ := f.st.InstanceByUUID("inst-stopped")
	if strings.Contains(stopped.DesiredStateJSON, "sleep_mode") {
		t.Fatalf("stopped instance spec was touched: %s", stopped.DesiredStateJSON)
	}

	// The default mode is wait, and it replaces a previously stamped mode.
	status, body = f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID)
	if status != http.StatusOK || body["mode"] != "wait" || body["level"] != float64(1) {
		t.Fatalf("default sleep: %d %v", status, body)
	}
	inst, _ = f.st.InstanceByUUID("inst-1")
	if !strings.Contains(inst.DesiredStateJSON, `"sleep_mode":"wait"`) {
		t.Fatalf("default mode not stamped: %s", inst.DesiredStateJSON)
	}
}

func TestAMDSleepOrdersRefuseButExistingSleepCanWake(t *testing.T) {
	f := newSleepFixture(t)
	if err := f.st.DB().Model(&store.GPU{}).Where("id = ?", f.node.GPUs[0].ID).Update("vendor", "AMD").Error; err != nil {
		t.Fatal(err)
	}
	dep := f.deployment(t, &store.Deployment{
		Name: "amd-sleeper", ServedName: "amd-sleeper", Engine: "vllm",
		Sleepable: true, DesiredSleep: 1,
	})
	f.instance(t, dep, "amd-sleeper-1", 1)

	status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID)
	params, _ := body["params"].(map[string]any)
	if status != http.StatusBadRequest || body["code"] != messages.ErrorAcceleratorFeatureUnsupported ||
		params["vendor"] != "AMD" || params["feature"] != "engine_sleep" {
		t.Fatalf("AMD sleep order = %d %v", status, body)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 1 {
		t.Fatalf("refused sleep order changed desired level to %d", got.DesiredSleep)
	}

	status, body = f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", dep.ID)
	if status != http.StatusOK || body["status"] != "waking" {
		t.Fatalf("wake of existing AMD sleeper = %d %v", status, body)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("wake did not restore desired level: %d", got.DesiredSleep)
	}
}

func TestWakeDeploymentRefusesWhileACoTenantHoldsTheCard(t *testing.T) {
	f := newSleepFixture(t)
	sleeper := f.deployment(t, &store.Deployment{Name: "sleeper", ServedName: "sleeper", Engine: "vllm", Sleepable: true, DesiredSleep: 1})
	f.instance(t, sleeper, "sleeper-1", 1)
	newcomer := f.deployment(t, &store.Deployment{Name: "newcomer", ServedName: "newcomer", Engine: "vllm"})
	f.instance(t, newcomer, "newcomer-1", 0)

	status, body := f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", sleeper.ID)
	if status != http.StatusConflict || body["code"] != "WAKE_INSUFFICIENT_GPU_MEMORY" {
		t.Fatalf("wake over a co-tenant: %d %v", status, body)
	}
	params := body["params"].(map[string]any)
	if params["node"] != "sleep-node" || params["shortfall_gib"] == "" || params["shortfall_gib"] == "0.0" {
		t.Fatalf("params = %v", params)
	}
	if got, _ := json.Marshal(params["gpu_indexes"]); string(got) != "[0]" {
		t.Fatalf("gpu_indexes = %s", got)
	}
	if got, _ := json.Marshal(params["occupants"]); string(got) != `["newcomer"]` {
		t.Fatalf("occupants = %s", got)
	}
	if got, _ := f.st.GetDeployment(sleeper.ID); got.DesiredSleep != 1 {
		t.Fatalf("refused wake changed the desired level to %d", got.DesiredSleep)
	}

	// Once the co-tenant is gone the same wake goes through.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "newcomer-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	status, body = f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", sleeper.ID)
	if status != http.StatusOK || body["status"] != "waking" {
		t.Fatalf("wake after co-tenant stopped: %d %v", status, body)
	}
	if got, _ := f.st.GetDeployment(sleeper.ID); got.DesiredSleep != 0 {
		t.Fatalf("desired level after wake = %d", got.DesiredSleep)
	}
}

func TestConcurrentWakesCannotBothBeAdmitted(t *testing.T) {
	f := newSleepFixture(t)
	a := f.deployment(t, &store.Deployment{Name: "sleep-a", ServedName: "sleep-a", Engine: "vllm", Sleepable: true, DesiredSleep: 1})
	b := f.deployment(t, &store.Deployment{Name: "sleep-b", ServedName: "sleep-b", Engine: "vllm", Sleepable: true, DesiredSleep: 1})
	f.instance(t, a, "sleep-a-1", 1)
	f.instance(t, b, "sleep-b-1", 1)
	type result struct {
		status int
		body   map[string]any
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, id := range []uint{a.ID, b.ID} {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			status, body := f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", id)
			results <- result{status: status, body: body}
		}(id)
	}
	wg.Wait()
	close(results)
	var accepted, refused int
	for got := range results {
		switch got.status {
		case http.StatusOK:
			accepted++
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("unexpected concurrent wake response %d: %+v", got.status, got.body)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent wakes accepted=%d refused=%d, want one each", accepted, refused)
	}
}

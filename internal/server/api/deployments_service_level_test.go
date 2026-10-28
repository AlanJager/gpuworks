package api

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type serviceLevelRig struct {
	*tokenRig
	node                         *store.Node
	plain, sleepy, multi, sglang *store.Deployment
}

func newServiceLevelRig(t *testing.T) *serviceLevelRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	node, err := rig.st.RegisterNode("sl-node", store.HashToken("sl"), "sl-node")
	if err != nil {
		t.Fatal(err)
	}
	create := func(dep *store.Deployment) *store.Deployment {
		dep.State = store.DeployActive
		dep.ServedName = dep.Name
		created, err := rig.st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	return &serviceLevelRig{
		tokenRig: rig,
		node:     node,
		plain:    create(&store.Deployment{Name: "plain", Engine: "vllm", Replicas: 1}),
		sleepy:   create(&store.Deployment{Name: "sleepy", Engine: "vllm", Sleepable: true, Replicas: 1}),
		multi:    create(&store.Deployment{Name: "multi", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1}),
		sglang:   create(&store.Deployment{Name: "sgl", Engine: "sglang", Replicas: 1}),
	}
}

// place puts a running instance of dep on the rig's node's cards.
func (r *serviceLevelRig) place(t *testing.T, dep *store.Deployment, uuid, gpus string) {
	t.Helper()
	if _, err := r.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: gpus}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceLevelRouteAuthAndTarget(t *testing.T) {
	rig := newServiceLevelRig(t)
	path := schedPath(rig.sleepy, "service-level")
	const body = `{"level":"offline"}`
	if rec := rig.do("", http.MethodPut, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s, want 401", rec.Code, rec.Body)
	}
	read := rig.mint("read")
	if rec := rig.do(read, http.MethodPut, path, body); rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
		t.Fatalf("read token = %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.mint("write"), http.MethodPut, path, body); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != body {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/deployments/9999/service-level", body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d %s, want 404", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if rec := rig.do(rig.session, method, path, "{}"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, rec.Code)
		}
	}
}

func TestServiceLevelSetting(t *testing.T) {
	rig := newServiceLevelRig(t)
	wire := func(dep *store.Deployment) (any, any) {
		t.Helper()
		got := (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, dep)
		return got["service_level"], got["sleep_held"]
	}
	if level, held := wire(rig.sleepy); level != "resident" || held != false {
		t.Fatalf("a new deployment reads service_level=%v sleep_held=%v, want resident/false", level, held)
	}
	path := schedPath(rig.sleepy, "service-level")
	for _, level := range []string{"online", "shared", "offline", "resident"} {
		body := `{"level":"` + level + `"}`
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != body {
			t.Fatalf("PUT %s = %d %s", body, rec.Code, rec.Body)
		}
		if got, _ := wire(rig.sleepy); got != level {
			t.Fatalf("after PUT %s the wire reads %v", level, got)
		}
	}

	for _, tc := range []struct {
		dep    *store.Deployment
		body   string
		status int
		code   string
	}{
		{rig.sleepy, `{"level":"gold"}`, http.StatusBadRequest, "SERVICE_LEVEL_INVALID"},
		{rig.sleepy, `{"level":""}`, http.StatusBadRequest, "SERVICE_LEVEL_INVALID"},
		{rig.plain, `{"level":"offline"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED"},
		{rig.sglang, `{"level":"shared"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED"},
		{rig.multi, `{"level":"online"}`, http.StatusBadRequest, "SERVICE_LEVEL_SHAPE"},
	} {
		rec := rig.do(rig.session, http.MethodPut, schedPath(tc.dep, "service-level"), tc.body)
		if rec.Code != tc.status || decodeBody(t, rec)["code"] != tc.code {
			t.Fatalf("%s %s = %d %s, want %d %s", tc.dep.Name, tc.body, rec.Code, rec.Body, tc.status, tc.code)
		}
	}
	// Resident needs nothing: any deployment can go back to it.
	for _, dep := range []*store.Deployment{rig.plain, rig.multi, rig.sglang} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "service-level"), `{"level":"resident"}`); rec.Code != http.StatusOK {
			t.Fatalf("%s back to resident = %d %s", dep.Name, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"level":"online","x":1}`, `{"level":1}`, `{`, ``} {
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", body, rec.Code)
		}
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.ServiceLevel != store.ServiceLevelResident {
		t.Fatalf("a refused request changed the level to %q", dep.ServiceLevel)
	}
}

func TestTwoOnlineDeploymentsMayNotShareACard(t *testing.T) {
	rig := newServiceLevelRig(t)
	other, err := rig.st.CreateDeployment(&store.Deployment{Name: "other", ServedName: "other", Engine: "vllm", Sleepable: true, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	rig.place(t, rig.sleepy, "sleepy-1", "[0,1]")
	rig.place(t, other, "other-1", "[1]")
	if rec := rig.do(rig.session, http.MethodPut, schedPath(other, "service-level"), `{"level":"online"}`); rec.Code != http.StatusOK {
		t.Fatalf("first online = %d %s", rec.Code, rec.Body)
	}
	rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "service-level"), `{"level":"online"}`)
	resp := decodeBody(t, rec)
	params, _ := resp["params"].(map[string]any)
	if rec.Code != http.StatusConflict || resp["code"] != "SERVICE_LEVEL_ONLINE_SHARED" || params["deployment"] != "other" {
		t.Fatalf("second online on a shared card = %d %s, want 409 naming other", rec.Code, rec.Body)
	}
	// Online may share with shared or offline.
	for _, level := range []string{"shared", "offline"} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "service-level"), `{"level":"`+level+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("%s next to online = %d %s", level, rec.Code, rec.Body)
		}
	}
	// A stopped instance holds no card.
	if err := rig.st.DB().Model(&store.Instance{}).Where("uuid = ?", "other-1").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "service-level"), `{"level":"online"}`); rec.Code != http.StatusOK {
		t.Fatalf("online once the other's instance stopped = %d %s", rec.Code, rec.Body)
	}
}

// An operator's sleep is held (a request does not wake it) and a wake lifts
// it; the deployment reads it as sleep_held.
func TestAnOperatorsSleepIsHeld(t *testing.T) {
	rig := newServiceLevelRig(t)
	rig.place(t, rig.sleepy, "sleepy-1", "[0]")
	if rec := rig.do(rig.session, http.MethodPost, schedPath(rig.sleepy, "sleep?level=2"), ""); rec.Code != http.StatusOK {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body)
	}
	got := (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, rig.sleepy)
	if got["sleep_held"] != true {
		t.Fatalf("after an operator's sleep sleep_held = %v", got["sleep_held"])
	}
}

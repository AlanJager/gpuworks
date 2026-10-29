package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// presetDeployment states the service level's policy on dep.
func presetDeployment(dep *store.Deployment, preset string) *store.Deployment {
	p, _ := servicelevel.PresetPolicy(preset)
	dep.SetPolicy(p)
	return dep
}

type yieldPolicyRig struct {
	*tokenRig
	node                         *store.Node
	plain, sleepy, multi, sglang *store.Deployment
}

func newYieldPolicyRig(t *testing.T) *yieldPolicyRig {
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
	return &yieldPolicyRig{
		tokenRig: rig,
		node:     node,
		plain:    create(&store.Deployment{Name: "plain", Engine: "vllm", Replicas: 1}),
		sleepy:   create(&store.Deployment{Name: "sleepy", Engine: "vllm", Sleepable: true, Replicas: 1}),
		multi:    create(&store.Deployment{Name: "multi", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1}),
		sglang:   create(&store.Deployment{Name: "sgl", Engine: "sglang", Replicas: 1}),
	}
}

// place puts a running instance of dep on the rig's node's cards.
func (r *yieldPolicyRig) place(t *testing.T, dep *store.Deployment, uuid, gpus string) {
	t.Helper()
	if _, err := r.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: gpus}); err != nil {
		t.Fatal(err)
	}
}

func TestYieldPolicyRouteAuthAndTarget(t *testing.T) {
	rig := newYieldPolicyRig(t)
	path := schedPath(rig.sleepy, "yield-policy")
	const body = `{"preset":"offline"}`
	const answer = `{"preset":"offline","policy":"anytime","auto_sleep":false,"sleep_level":"auto","unfinished":"abort","hold_max_sec":1800}`
	if rec := rig.do("", http.MethodPut, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s, want 401", rec.Code, rec.Body)
	}
	read := rig.mint("read")
	if rec := rig.do(read, http.MethodPut, path, body); rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
		t.Fatalf("read token = %d %s, want 403", rec.Code, rec.Body)
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Preset() != servicelevel.PresetResident {
		t.Fatalf("a refused request changed the policy to %s", dep.Preset())
	}
	if rec := rig.do(rig.mint("write"), http.MethodPut, path, body); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != answer {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/deployments/9999/yield-policy", body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d %s, want 404", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if rec := rig.do(rig.session, method, path, "{}"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, rec.Code)
		}
	}
}

// The two routes the yield policy replaced are gone, not kept as aliases.
func TestTheServiceLevelAndIdleSleepRoutesAreGone(t *testing.T) {
	rig := newYieldPolicyRig(t)
	for _, tc := range []struct{ tail, body string }{
		{"service-level", `{"level":"offline"}`},
		{"idle-sleep", `{"after_sec":900,"level":2}`},
	} {
		for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodPost} {
			if rec := rig.do(rig.session, method, schedPath(rig.sleepy, tc.tail), tc.body); rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d %s, want 404", method, tc.tail, rec.Code, rec.Body)
			}
		}
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Preset() != servicelevel.PresetResident || dep.AutoSleep {
		t.Fatalf("a removed route changed the policy: %+v", dep.Policy())
	}
}

func TestYieldPolicySetting(t *testing.T) {
	rig := newYieldPolicyRig(t)
	wire := func(dep *store.Deployment) map[string]any {
		t.Helper()
		return (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, dep)
	}
	got := wire(rig.sleepy)
	if jsonOf(t, got["yield_policy"]) != `{"auto_sleep":false,"policy":"never","preset":"resident"}` || got["sleep_held"] != false {
		t.Fatalf("a new deployment reads yield_policy=%s sleep_held=%v, want resident/false", jsonOf(t, got["yield_policy"]), got["sleep_held"])
	}
	for _, gone := range []string{"service_level", "idle_sleep"} {
		if _, ok := got[gone]; ok {
			t.Fatalf("the deployment still carries %s", gone)
		}
	}
	path := schedPath(rig.sleepy, "yield-policy")
	// Every body is answered with the policy as stored, and the deployment
	// then reads exactly that.
	for _, tc := range []struct{ name, body, want string }{
		{"online preset", `{"preset":"online"}`, `{"preset":"online","policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}`},
		{"shared preset", `{"preset":"shared"}`, `{"preset":"shared","policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		{"offline preset", `{"preset":"offline"}`, `{"preset":"offline","policy":"anytime","auto_sleep":false,"sleep_level":"auto","unfinished":"abort","hold_max_sec":1800}`},
		{"resident preset", `{"preset":"resident"}`, `{"preset":"resident","policy":"never","auto_sleep":false}`},
		// What the UI sends for a preset's full parameter set is the preset.
		{"online as a full object", `{"policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}`, `{"preset":"online","policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}`},
		{"shared as a full object", `{"policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`, `{"preset":"shared","policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		// A policy alone takes its defaults.
		{"policy alone", `{"policy":"timeslice"}`, `{"preset":"shared","policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		// A change from a preset is custom.
		{"idle with a 30 s idle time", `{"policy":"idle","idle_sec":30}`, `{"preset":"custom","policy":"idle","auto_sleep":false,"idle_sec":30,"sleep_level":"auto","hold_max_sec":120}`},
		{"never with auto sleep", `{"policy":"never","auto_sleep":true,"idle_sec":600,"sleep_level":"l1"}`, `{"preset":"custom","policy":"never","auto_sleep":true,"idle_sec":600,"sleep_level":"l1","hold_max_sec":120}`},
		{"anytime that waits and sleeps on its own", `{"policy":"anytime","auto_sleep":true,"idle_sec":45,"sleep_level":"l2","unfinished":"wait","unfinished_wait_sec":90,"hold_max_sec":60}`, `{"preset":"custom","policy":"anytime","auto_sleep":true,"idle_sec":45,"sleep_level":"l2","unfinished":"wait","unfinished_wait_sec":90,"hold_max_sec":60}`},
		{"timeslice that waits without a bound", `{"policy":"timeslice","unfinished":"wait_unbounded","min_serve_sec":0}`, `{"preset":"custom","policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":0,"unfinished":"wait_unbounded","hold_max_sec":600}`},
		// Parameters that do not apply are ignored, however they are spelt.
		{"inapplicable parameters", `{"policy":"idle","idle_sec":300,"min_serve_sec":99999,"unfinished":"abort","unfinished_wait_sec":0}`, `{"preset":"online","policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}`},
		{"never ignores everything but auto sleep", `{"policy":"never","idle_sec":5,"sleep_level":"l2","hold_max_sec":1}`, `{"preset":"resident","policy":"never","auto_sleep":false}`},
		// A response sent back as it is, its derived preset ignored.
		{"a response sent back", `{"preset":"online","policy":"timeslice","auto_sleep":true,"idle_sec":100,"sleep_level":"l1","min_serve_sec":5,"unfinished":"wait","unfinished_wait_sec":9,"hold_max_sec":15}`, `{"preset":"custom","policy":"timeslice","auto_sleep":true,"idle_sec":100,"sleep_level":"l1","min_serve_sec":5,"unfinished":"wait","unfinished_wait_sec":9,"hold_max_sec":15}`},
		{"bounds", `{"policy":"timeslice","idle_sec":30,"min_serve_sec":3600,"unfinished":"wait","unfinished_wait_sec":600,"hold_max_sec":10}`, `{"preset":"custom","policy":"timeslice","auto_sleep":false,"idle_sec":30,"sleep_level":"auto","min_serve_sec":3600,"unfinished":"wait","unfinished_wait_sec":600,"hold_max_sec":10}`},
		{"upper bounds", `{"policy":"idle","idle_sec":604800,"hold_max_sec":1800}`, `{"preset":"custom","policy":"idle","auto_sleep":false,"idle_sec":604800,"sleep_level":"auto","hold_max_sec":1800}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.do(rig.session, http.MethodPut, path, tc.body)
			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.want {
				t.Fatalf("PUT %s = %d %s\nwant %s", tc.body, rec.Code, rec.Body, tc.want)
			}
			var stated, read map[string]any
			_ = json.Unmarshal([]byte(tc.want), &stated)
			if read, _ = wire(rig.sleepy)["yield_policy"].(map[string]any); jsonOf(t, read) != jsonOf(t, stated) {
				t.Fatalf("the deployment reads %s after %s, want %s", jsonOf(t, read), tc.body, jsonOf(t, stated))
			}
		})
	}

	for _, tc := range []struct {
		dep    *store.Deployment
		body   string
		status int
		code   string
		params map[string]any
	}{
		{rig.sleepy, `{}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "required"}},
		{rig.sleepy, `{"preset":"online","idle_sec":60}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "required"}},
		{rig.sleepy, `{"preset":"custom"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "preset", "reason": "invalid_value"}},
		{rig.sleepy, `{"preset":"gold"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "preset", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"gold"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"idle","sleep_level":"l3"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "sleep_level", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"never","unfinished":"maybe"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "unfinished", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"idle","idle_sec":29}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "idle_sec", "reason": "out_of_range", "minimum": float64(30), "maximum": float64(604800)}},
		{rig.sleepy, `{"policy":"timeslice","idle_sec":604801}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "idle_sec", "reason": "out_of_range", "minimum": float64(30), "maximum": float64(604800)}},
		{rig.sleepy, `{"policy":"never","auto_sleep":true,"idle_sec":10}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "idle_sec", "reason": "out_of_range", "minimum": float64(30), "maximum": float64(604800)}},
		{rig.sleepy, `{"policy":"timeslice","min_serve_sec":3601}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "min_serve_sec", "reason": "out_of_range", "minimum": float64(0), "maximum": float64(3600)}},
		{rig.sleepy, `{"policy":"anytime","unfinished":"wait","unfinished_wait_sec":0}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "unfinished_wait_sec", "reason": "out_of_range", "minimum": float64(1), "maximum": float64(600)}},
		{rig.sleepy, `{"policy":"timeslice","unfinished":"wait","unfinished_wait_sec":601}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "unfinished_wait_sec", "reason": "out_of_range", "minimum": float64(1), "maximum": float64(600)}},
		{rig.sleepy, `{"policy":"idle","hold_max_sec":9}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "hold_max_sec", "reason": "out_of_range", "minimum": float64(10), "maximum": float64(1800)}},
		{rig.sleepy, `{"policy":"anytime","hold_max_sec":1801}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "hold_max_sec", "reason": "out_of_range", "minimum": float64(10), "maximum": float64(1800)}},
		{rig.plain, `{"preset":"offline"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED", nil},
		{rig.plain, `{"policy":"never","auto_sleep":true}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED", nil},
		{rig.sglang, `{"preset":"shared"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED", nil},
		{rig.multi, `{"preset":"online"}`, http.StatusBadRequest, "YIELD_POLICY_SHAPE", nil},
		{rig.multi, `{"policy":"anytime","auto_sleep":true}`, http.StatusBadRequest, "YIELD_POLICY_SHAPE", nil},
	} {
		rec := rig.do(rig.session, http.MethodPut, schedPath(tc.dep, "yield-policy"), tc.body)
		resp := decodeBody(t, rec)
		if rec.Code != tc.status || resp["code"] != tc.code {
			t.Fatalf("%s %s = %d %s, want %d %s", tc.dep.Name, tc.body, rec.Code, rec.Body, tc.status, tc.code)
		}
		if tc.params != nil {
			if got, _ := resp["params"].(map[string]any); jsonOf(t, got) != jsonOf(t, tc.params) {
				t.Fatalf("%s %s params = %s, want %s", tc.dep.Name, tc.body, jsonOf(t, got), jsonOf(t, tc.params))
			}
		}
	}
	// Never needs nothing: any deployment can go back to it.
	for _, dep := range []*store.Deployment{rig.plain, rig.multi, rig.sglang} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "yield-policy"), `{"preset":"resident"}`); rec.Code != http.StatusOK {
			t.Fatalf("%s back to resident = %d %s", dep.Name, rec.Code, rec.Body)
		}
	}
	// Auto sleep, which the old idle sleep allowed on any deployment that can
	// sleep, is allowed on a multi-node one.
	if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.multi, "yield-policy"), `{"policy":"never","auto_sleep":true,"idle_sec":900,"sleep_level":"l2"}`); rec.Code != http.StatusOK {
		t.Fatalf("auto sleep on a multi-node deployment = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"preset":"online","x":1}`, `{"preset":1}`, `{"policy":"idle","idle_sec":"60"}`, `{"policy":"idle","auto_sleep":"yes"}`, `{`, ``} {
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", body, rec.Code)
		}
	}
	// Every refusal left the last accepted policy: the bounds test's.
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Policy().IdleSec != 604800 || dep.Policy().HoldMaxSec != 1800 || dep.Policy().Yield != servicelevel.Idle {
		t.Fatalf("a refused request changed the policy: %+v", dep.Policy())
	}
}

func TestTwoIdleDeploymentsMayNotShareACard(t *testing.T) {
	rig := newYieldPolicyRig(t)
	other, err := rig.st.CreateDeployment(&store.Deployment{Name: "other", ServedName: "other", Engine: "vllm", Sleepable: true, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	rig.place(t, rig.sleepy, "sleepy-1", "[0,1]")
	rig.place(t, other, "other-1", "[1]")
	if rec := rig.do(rig.session, http.MethodPut, schedPath(other, "yield-policy"), `{"preset":"online"}`); rec.Code != http.StatusOK {
		t.Fatalf("first idle = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"preset":"online"}`, `{"policy":"idle","idle_sec":30}`} {
		rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), body)
		resp := decodeBody(t, rec)
		params, _ := resp["params"].(map[string]any)
		if rec.Code != http.StatusConflict || resp["code"] != "YIELD_POLICY_IDLE_SHARED" || params["deployment"] != "other" {
			t.Fatalf("second idle %s on a shared card = %d %s, want 409 naming other", body, rec.Code, rec.Body)
		}
	}
	// Idle may share with timeslice or anytime, and a never one with auto sleep.
	for _, body := range []string{`{"preset":"shared"}`, `{"preset":"offline"}`, `{"policy":"never","auto_sleep":true}`, `{"policy":"anytime","auto_sleep":true}`} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), body); rec.Code != http.StatusOK {
			t.Fatalf("%s next to idle = %d %s", body, rec.Code, rec.Body)
		}
	}
	// A stopped instance holds no card.
	if err := rig.st.DB().Model(&store.Instance{}).Where("uuid = ?", "other-1").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), `{"preset":"online"}`); rec.Code != http.StatusOK {
		t.Fatalf("idle once the other's instance stopped = %d %s", rec.Code, rec.Body)
	}
}

// An operator's sleep is held (a request does not wake it) and a wake lifts
// it; the deployment reads it as sleep_held.
func TestAnOperatorsSleepIsHeld(t *testing.T) {
	rig := newYieldPolicyRig(t)
	rig.place(t, rig.sleepy, "sleepy-1", "[0]")
	if rec := rig.do(rig.session, http.MethodPost, schedPath(rig.sleepy, "sleep?level=2"), ""); rec.Code != http.StatusOK {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body)
	}
	got := (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, rig.sleepy)
	if got["sleep_held"] != true {
		t.Fatalf("after an operator's sleep sleep_held = %v", got["sleep_held"])
	}
}

func TestANewDeploymentsYieldPolicyIsCheckedLikeAChange(t *testing.T) {
	policy := func(raw string) *dto.YieldPolicyRequest {
		var r dto.YieldPolicyRequest
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	for _, tc := range []struct {
		name   string
		req    dto.DeployRequest
		pd     bool
		ok     bool
		code   string
		stored string
	}{
		{name: "none stated", req: dto.DeployRequest{}, ok: true, stored: "resident"},
		{name: "offline", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"offline"}`), Sleepable: true}, ok: true, stored: "offline"},
		{name: "a custom policy", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"idle","idle_sec":30}`), Sleepable: true}, ok: true, stored: "custom"},
		{name: "unknown preset", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"gold"}`), Sleepable: true}, code: "YIELD_POLICY_INVALID"},
		{name: "empty object", req: dto.DeployRequest{YieldPolicy: policy(`{}`), Sleepable: true}, code: "YIELD_POLICY_INVALID"},
		{name: "not sleepable", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"online"}`)}, code: "SLEEP_NOT_ENABLED"},
		{name: "auto sleep not sleepable", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"never","auto_sleep":true}`)}, code: "SLEEP_NOT_ENABLED"},
		{name: "multi-node", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"shared"}`), Sleepable: true, NNodes: 2}, code: "YIELD_POLICY_SHAPE"},
		{name: "pd", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"shared"}`), Sleepable: true}, pd: true, code: "YIELD_POLICY_SHAPE"},
		{name: "resident multi-node", req: dto.DeployRequest{YieldPolicy: policy(`{"preset":"resident"}`), NNodes: 2}, ok: true, stored: "resident"},
		{name: "auto sleep multi-node", req: dto.DeployRequest{YieldPolicy: policy(`{"policy":"never","auto_sleep":true}`), Sleepable: true, NNodes: 2}, ok: true, stored: "custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			p, ok := createYieldPolicy(rec, tc.req, "vllm", tc.pd)
			if ok != tc.ok {
				t.Fatalf("ok = %v (%s)", ok, rec.Body)
			}
			if !ok {
				if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["code"] != tc.code {
					t.Fatalf("%d %s, want 400 %s", rec.Code, rec.Body, tc.code)
				}
				return
			}
			if got := servicelevel.PresetOf(p); got != tc.stored {
				t.Fatalf("stored as %q, want %q", got, tc.stored)
			}
			// What placement previews plan for is the stored policy's yield.
			if got, want := requestYield(tc.req.YieldPolicy), p.Yield; got != want {
				t.Fatalf("requestYield = %q, want %q", got, want)
			}
		})
	}
	if requestYield(policy(`{"preset":"gold"}`)) != servicelevel.Never {
		t.Fatal("a malformed policy plans as never")
	}
}

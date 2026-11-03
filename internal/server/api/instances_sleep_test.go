package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replicaSleepFixture is the sleep fixture's node with a second card and a
// sleepable two-replica deployment, one replica per card.
func replicaSleepFixture(t *testing.T) (sleepFixture, *store.Deployment) {
	t.Helper()
	f := newSleepFixture(t)
	gpu := store.GPU{Index: 1, UUID: "gpu-1", NodeID: f.node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 24 << 30}
	if err := f.st.DB().Create(&gpu).Error; err != nil {
		t.Fatal(err)
	}
	dep := f.deployment(t, &store.Deployment{Name: "replicas", ServedName: "replicas", Engine: "vllm", Sleepable: true, Replicas: 2})
	for i, uuid := range []string{"plain-0", "plain-1"} {
		inst := f.instance(t, dep, uuid, 0)
		if err := f.st.DB().Model(&store.Instance{}).Where("id = ?", inst.ID).Update("GPUIndexes", []string{"[0]", "[1]"}[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	return f, dep
}

func sleepIntent(t *testing.T, st *store.Store, uuid string) int {
	t.Helper()
	inst, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	return spec.DesiredSleep
}

func reportAsleep(t *testing.T, st *store.Store, uuid string, level int) {
	t.Helper()
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: level, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
}

// occupy puts an awake whole-card instance of another deployment on a card.
func (f sleepFixture) occupy(t *testing.T, gpuIndexes string) {
	t.Helper()
	other := f.deployment(t, &store.Deployment{Name: "newcomer", ServedName: "newcomer-" + gpuIndexes, Engine: "vllm"})
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "newcomer-" + gpuIndexes, DeploymentID: other.ID, NodeID: f.node.ID, State: proto.InstRunning, GPUIndexes: gpuIndexes}); err != nil {
		t.Fatal(err)
	}
}

func TestSleepInstanceSleepsOneReplicaAndWakesIt(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	rec := instanceRequest(f.h.sleepInstance, "plain-0", "?level=2&mode=abort")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"sleeping","level":2,"mode":"abort","instances":["plain-0"],"wake_on_request":false}` {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body.String())
	}
	if a, b := sleepIntent(t, f.st, "plain-0"), sleepIntent(t, f.st, "plain-1"); a != 2 || b != 0 {
		t.Fatalf("intents = %d/%d, want 2/0", a, b)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("one replica's order set the deployment level to %d", got.DesiredSleep)
	}
	// The instance on the wire carries the order next to the reported level.
	if rec := instanceRequest(f.h.getInstance, "plain-0", ""); !strings.Contains(rec.Body.String(), `"sleep_level":0,"desired_sleep":2,`) {
		t.Fatalf("instance DTO = %s", rec.Body.String())
	}
	reportAsleep(t, f.st, "plain-0", 2)
	rec = instanceRequest(f.h.wakeInstance, "plain-0", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"waking","instances":["plain-0"]}` {
		t.Fatalf("wake = %d %s", rec.Code, rec.Body.String())
	}
	if got := sleepIntent(t, f.st, "plain-0"); got != 0 {
		t.Fatalf("woken intent = %d", got)
	}
}

// A wake of one replica checks only its own card: another card being taken
// does not stop it, its own card being taken does.
func TestWakeInstanceAdmitsOnlyItsOwnCard(t *testing.T) {
	f, _ := replicaSleepFixture(t)
	for _, uuid := range []string{"plain-0", "plain-1"} {
		if rec := instanceRequest(f.h.sleepInstance, uuid, ""); rec.Code != http.StatusOK {
			t.Fatalf("sleep %s = %d %s", uuid, rec.Code, rec.Body.String())
		}
		reportAsleep(t, f.st, uuid, 1)
	}
	f.occupy(t, "[1]")
	rec := instanceRequest(f.h.wakeInstance, "plain-1", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorWakeInsufficientGPUMemory+`"`) || !strings.Contains(rec.Body.String(), `"gpu_indexes":[1]`) {
		t.Fatalf("wake onto a taken card = %d %s", rec.Code, rec.Body.String())
	}
	if got := sleepIntent(t, f.st, "plain-1"); got != 1 {
		t.Fatalf("a refused wake changed the intent to %d", got)
	}
	if rec := instanceRequest(f.h.wakeInstance, "plain-0", ""); rec.Code != http.StatusOK {
		t.Fatalf("wake onto its free card = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSleepInstanceRefusals(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	plainDep := f.deployment(t, &store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm", Replicas: 1})
	f.instance(t, plainDep, "not-sleepable", 0)
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "plain-failed", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstFailed, DesiredStateJSON: `{"uuid":"plain-failed"}`}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, uuid, query string
		status            int
		code              string
	}{
		{name: "unknown instance", uuid: "nope", status: http.StatusNotFound},
		{name: "deployment not sleepable", uuid: "not-sleepable", status: http.StatusBadRequest, code: ErrorSleepNotEnabled},
		{name: "failed attempt", uuid: "plain-failed", status: http.StatusConflict, code: ErrorInstanceNotServing},
		{name: "bad level", uuid: "plain-0", query: "?level=3", status: http.StatusBadRequest, code: ErrorSleepInvalidLevel},
		{name: "bad mode", uuid: "plain-0", query: "?mode=keep", status: http.StatusBadRequest, code: ErrorSleepInvalidMode},
	} {
		rec := instanceRequest(f.h.sleepInstance, tc.uuid, tc.query)
		if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if got := sleepIntent(t, f.st, "plain-0"); got != 0 {
		t.Fatalf("a refused order changed the intent to %d", got)
	}
}

// A multi-node replica sleeps as one: its leader's engine takes its
// followers down with it. An order naming either rank is the replica's and is
// written on both; the deployment's state names both as asleep.
func TestSleepingAMultiNodeReplicaOrdersEveryRank(t *testing.T) {
	h, st, _, multi := replicaFixture(t)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", multi.ID).Updates(map[string]any{"sleepable": true, "engine": "vllm"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"multi-leader", "multi-follower"} {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, Headless: uuid == "multi-follower"})
		if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Update("spec", string(raw)).Error; err != nil {
			t.Fatal(err)
		}
	}
	rec := instanceRequest(h.sleepInstance, "multi-follower", "?level=1")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"sleeping","level":1,"mode":"wait","instances":["multi-follower","multi-leader"],"wake_on_request":false}`+"\n" {
		t.Fatalf("sleep through the follower = %d %s", rec.Code, rec.Body.String())
	}
	for _, uuid := range []string{"multi-leader", "multi-follower"} {
		if got := sleepIntent(t, st, uuid); got != 1 {
			t.Fatalf("%s intent = %d, want 1", uuid, got)
		}
	}
	if dep, err := st.GetDeployment(multi.ID); err != nil || dep.DesiredSleep != 0 {
		t.Fatalf("one replica's order put the deployment under a sleep order: %+v %v", dep, err)
	}
	// The leader's engine reports the replica asleep; the follower reports
	// nothing of its own and reads at the leader's level.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "multi-leader", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	rec = instanceRequest(h.getInstance, "multi-follower", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sleep_level":1,"desired_sleep":1,"asleep":true`) {
		t.Fatalf("follower of an asleep leader = %d %s, want its leader's level", rec.Code, rec.Body.String())
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "multi-leader", State: proto.InstRunning, SleepLevel: 0}); err != nil {
		t.Fatal(err)
	}
	rec = instanceRequest(h.wakeInstance, "multi-leader", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"waking","instances":["multi-follower","multi-leader"]}`+"\n" {
		t.Fatalf("wake through the leader = %d %s", rec.Code, rec.Body.String())
	}
	for _, uuid := range []string{"multi-leader", "multi-follower"} {
		if got := sleepIntent(t, st, uuid); got != 0 {
			t.Fatalf("%s intent after wake = %d", uuid, got)
		}
	}
}

// pdSleepFixture is the PD fixture's deployment made sleepable, with its two
// prefill replicas and one decode replica running.
func pdSleepFixture(t *testing.T) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	h, st, created := createPDForOps(t)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", created.ID).Update("sleepable", true).Error; err != nil {
		t.Fatal(err)
	}
	nodeA, _ := st.NodeByUUID("pd-a")
	nodeB, _ := st.NodeByUUID("pd-b")
	for _, row := range []store.Instance{
		{UUID: "prefill-0", NodeID: nodeA.ID, GPUIndexes: "[0]", Role: "prefill"},
		{UUID: "prefill-1", NodeID: nodeA.ID, GPUIndexes: "[1]", Role: "prefill"},
		{UUID: "decode-0", NodeID: nodeB.ID, GPUIndexes: "[0,1]", Role: "decode"},
	} {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: row.UUID, DesiredState: "running", Sleepable: true})
		row.DeploymentID, row.State, row.DesiredStateJSON = created.ID, proto.InstRunning, string(raw)
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	return h, st, created.ID
}

// A PD deployment's prefill role sleeps on its own, which the mode runs
// without; decode, which it cannot run without, sleeps only with the whole
// deployment, by role and by its last awake instance alike.
func TestSleepByRole(t *testing.T) {
	h, st, id := pdSleepFixture(t)
	req := func(query string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/"+query, nil)
		r = r.WithContext(chiRouteContext(r.Context(), map[string]string{"id": strconv.FormatUint(uint64(id), 10)}))
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec
	}
	got := req("?role=prefill", h.sleepDeployment)
	if got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != `{"status":"sleeping","level":1,"mode":"wait","role":"prefill","instances":["prefill-0","prefill-1"],"wake_on_request":false}` {
		t.Fatalf("sleep prefill = %d %s", got.Code, got.Body.String())
	}
	if p0, p1, d := sleepIntent(t, st, "prefill-0"), sleepIntent(t, st, "prefill-1"), sleepIntent(t, st, "decode-0"); p0 != 1 || p1 != 1 || d != 0 {
		t.Fatalf("intents prefill %d/%d decode %d", p0, p1, d)
	}
	if dep, _ := st.GetDeployment(id); dep.DesiredSleep != 0 {
		t.Fatalf("a role order set the deployment level to %d", dep.DesiredSleep)
	}
	for query, code := range map[string]string{
		"?role=decode": ErrorSleepRoleCannotEmpty,
		"?role=encode": messages.ErrorPDRoleUnknown,
	} {
		if got := req(query, h.sleepDeployment); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s = %d %s", query, got.Code, got.Body.String())
		}
	}
	// The client's text for an unknown role names the mode too.
	if got := req("?role=encode", h.sleepDeployment); !strings.Contains(got.Body.String(), `"params":{"mode":"vllm-nixl","role":"encode"}`) {
		t.Fatalf("unknown role params = %s", got.Body.String())
	}
	if rec := instanceRequest(h.sleepInstance, "decode-0", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorSleepRoleCannotEmpty+`"`) {
		t.Fatalf("last decode = %d %s", rec.Code, rec.Body.String())
	}
	if got := req("?role=prefill", h.wakeDeployment); got.Code != http.StatusOK {
		t.Fatalf("wake prefill = %d %s", got.Code, got.Body.String())
	}
	if p0, p1 := sleepIntent(t, st, "prefill-0"), sleepIntent(t, st, "prefill-1"); p0 != 0 || p1 != 0 {
		t.Fatalf("woken prefill intents %d/%d", p0, p1)
	}
}

func TestSleepByRoleRefusesAnOrdinaryDeployment(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?role=prefill", dep.ID)
	if status != http.StatusBadRequest || body["code"] != messages.ErrorPDNotPD {
		t.Fatalf("role on an ordinary deployment = %d %v", status, body)
	}
}

// A partial wake wakes what fits and lists the rest; an all-or-nothing wake
// of the same refuses and changes nothing.
func TestPartialWakeWakesWhatFits(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID); status != http.StatusOK {
		t.Fatalf("sleep = %d %v", status, body)
	}
	reportAsleep(t, f.st, "plain-0", 1)
	reportAsleep(t, f.st, "plain-1", 1)
	f.occupy(t, "[1]")
	status, body := f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", dep.ID)
	if status != http.StatusConflict || body["code"] != ErrorWakeInsufficientGPUMemory {
		t.Fatalf("all-or-nothing wake = %d %v", status, body)
	}
	if a, b := sleepIntent(t, f.st, "plain-0"), sleepIntent(t, f.st, "plain-1"); a != 1 || b != 1 {
		t.Fatalf("a refused wake changed intents to %d/%d", a, b)
	}
	status, body = f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake?partial=true", dep.ID)
	if status != http.StatusOK {
		t.Fatalf("partial wake = %d %v", status, body)
	}
	raw, _ := json.Marshal(body)
	if string(raw) != `{"refused":[{"gpu_indexes":[1],"instances":["plain-1"],"node":"sleep-node","occupants":["newcomer"],"shortfall_gib":"21.6"}],"status":"waking","woken":["plain-0"]}` {
		t.Fatalf("partial wake body = %s", raw)
	}
	if a, b := sleepIntent(t, f.st, "plain-0"), sleepIntent(t, f.st, "plain-1"); a != 0 || b != 1 {
		t.Fatalf("partial wake intents = %d/%d, want 0/1", a, b)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("partial wake left the deployment-wide order at %d", got.DesiredSleep)
	}
	if status, _ := f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake?partial=maybe", dep.ID); status != http.StatusBadRequest {
		t.Fatalf("unreadable partial = %d", status)
	}

	// Nothing that fits: refused as a whole, every refusal listed.
	if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	reportAsleep(t, f.st, "plain-0", 1)
	f.occupy(t, "[0]")
	status, body = f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake?partial=true", dep.ID)
	refusedList, _ := body["params"].(map[string]any)["refused"].([]any)
	if status != http.StatusConflict || body["code"] != ErrorWakeInsufficientGPUMemory || len(refusedList) != 2 {
		t.Fatalf("partial wake with nothing that fits = %d %v", status, body)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 1 {
		t.Fatalf("a refused wake withdrew the deployment-wide order: %d", got.DesiredSleep)
	}
}

// A partial wake with no sleeper to take back still withdraws a
// deployment-wide order, so replicas rebuilt afterwards start awake.
func TestPartialWakeWithNothingAsleepWithdrawsTheDeploymentOrder(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	status, body := f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake?partial=true", dep.ID)
	if raw, _ := json.Marshal(body); status != http.StatusOK || string(raw) != `{"status":"waking","woken":["plain-0","plain-1"]}` {
		t.Fatalf("partial wake of ordered-only sleepers = %d %s", status, raw)
	}
	if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("deployment-wide order = %d after the wake, want withdrawn", got.DesiredSleep)
	}
}

func instanceWire(t *testing.T, h *apiHandlers, uuid string) (dto.Instance, string) {
	t.Helper()
	rec := instanceRequest(h.getInstance, uuid, "")
	var out dto.Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("instance %s: %v %s", uuid, err, rec.Body.String())
	}
	return out, rec.Body.String()
}

// The instance on the wire says what the server decided about it: the replica
// it fills, whether the gateway holds it back for sleep, and whether it takes
// a sleep or wake order of its own; the UI shows buttons from these alone.
func TestInstanceDTOCarriesTheServersReplicaAndSleepVerdicts(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	got, raw := instanceWire(t, f.h, "plain-0")
	if got.Replica == nil || got.Replica.Role != "" || got.Asleep || !got.CanSleepAlone || !got.CanWakeAlone {
		t.Fatalf("awake replica = %s", raw)
	}
	for _, key := range []string{`"asleep":false`, `"replica":{"index":`, `"can_sleep_alone":true`, `"can_wake_alone":true`} {
		if !strings.Contains(raw, key) {
			t.Fatalf("instance lacks %s: %s", key, raw)
		}
	}
	if err := f.st.OrderInstancesSleep(dep.ID, []string{"plain-0"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	if got, raw := instanceWire(t, f.h, "plain-0"); !got.Asleep || got.SleepLevel != 0 || got.DesiredSleep != 1 {
		t.Fatalf("ordered to sleep = %s, want asleep at once", raw)
	}

	h, _, id := pdSleepFixture(t)
	if got, raw := instanceWire(t, h, "decode-0"); got.Replica == nil || got.Replica.Role != "decode" || got.CanSleepAlone || !got.CanWakeAlone {
		t.Fatalf("the only decode = %s, want it unable to sleep alone", raw)
	}
	if got, raw := instanceWire(t, h, "prefill-1"); got.Replica == nil || got.Replica.Role != "prefill" || !got.CanSleepAlone {
		t.Fatalf("a prefill = %s", raw)
	}
	rec := sleepCallByID(h.getDeployment, id)
	var deployment dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &deployment); err != nil || deployment.PD == nil {
		t.Fatalf("PD deployment = %s", rec.Body.String())
	}
	for _, role := range deployment.PD.Roles {
		if role.MayBeEmpty != (role.Role == "prefill") {
			t.Fatalf("role %s may_be_empty = %v", role.Role, role.MayBeEmpty)
		}
	}
}

// A follower fills its leader's replica but takes no sleep order of its own;
// a row from before rank roles were recorded reads as the gateway reads it.
func TestInstanceDTONamesAFollowersReplicaAndItsRole(t *testing.T) {
	h, st, _, multi := replicaFixture(t)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", multi.ID).Updates(map[string]any{"sleepable": true, "engine": "vllm"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "multi-follower").Update("dist_role", "").Error; err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"multi-leader", "multi-follower"} {
		got, raw := instanceWire(t, h, uuid)
		if got.Replica == nil || got.CanSleepAlone || got.CanWakeAlone {
			t.Fatalf("%s = %s, want its replica and no order of its own", uuid, raw)
		}
	}
	if got, raw := instanceWire(t, h, "multi-follower"); got.DistRole != "follower" {
		t.Fatalf("a legacy follower row = %s, want dist_role follower", raw)
	}
}

func sleepCallByID(handler http.HandlerFunc, id uint) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(chiRouteContext(r.Context(), map[string]string{"id": strconv.FormatUint(uint64(id), 10)}))
	rec := httptest.NewRecorder()
	handler(rec, r)
	return rec
}

// An order whose replica lost a member between the read and the write is the
// replicas changing under the request: 409 REPLICAS_CHANGED, not a 500.
func TestAnOrderForAReplicaThatChangedIsAConflict(t *testing.T) {
	rec := httptest.NewRecorder()
	writeReplicaOrderErr(rec, store.ErrNotFound)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorReplicasChanged+`"`) {
		t.Fatalf("member gone = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeReplicaOrderErr(rec, errors.New("disk full"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentWireOf is GET /deployments/{id}'s body.
func (f sleepFixture) deploymentWireOf(t *testing.T, id uint) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/x", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": strconv.FormatUint(uint64(id), 10)}))
	rec := httptest.NewRecorder()
	f.h.getDeployment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deployment = %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The hold is each instance's, on the wire too: the instance says its own
// sleep_held, and the deployment's sleep_hold and sleep_held are derived from
// its asleep replicas - all held, some, or none - so a mix is told apart.
func TestSleepHeldOnTheWire(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	wire := func(step string, want ...string) dto.Deployment {
		t.Helper()
		body := f.deploymentWireOf(t, dep.ID)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Fatalf("%s: deployment wire lacks %s:\n%s", step, w, body)
			}
		}
		var got dto.Deployment
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	instanceWire := func(uuid string) string {
		t.Helper()
		return instanceRequest(f.h.getInstance, uuid, "").Body.String()
	}

	// Awake: no sleep_hold key, sleep_held false.
	if got := wire("awake", `"sleep_held":false`); got.SleepHold != "" || strings.Contains(f.deploymentWireOf(t, dep.ID), `"sleep_hold"`) {
		t.Fatalf("an awake deployment has sleep_hold %q", got.SleepHold)
	}

	// The operator sleeps plain-0; the arbiter puts plain-1 to sleep.
	if rec := instanceRequest(f.h.sleepInstance, "plain-0", "?level=2"); rec.Code != http.StatusOK {
		t.Fatalf("sleep plain-0 = %d %s", rec.Code, rec.Body)
	}
	if err := f.st.OrderInstancesSleep(dep.ID, []string{"plain-1"}, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	got := wire("operator + arbiter", `"sleep_hold":"some","sleep_held":false`)
	if got.SleepHold != dto.SleepHoldSome || got.SleepHeld || got.AsleepReplicas != 2 {
		t.Fatalf("mixed = hold %q held %v asleep %d", got.SleepHold, got.SleepHeld, got.AsleepReplicas)
	}
	held := map[string]bool{}
	for _, in := range got.Instances {
		held[in.UUID] = in.SleepHeld
	}
	if !held["plain-0"] || held["plain-1"] {
		t.Fatalf("instance holds = %v, want plain-0 alone", held)
	}
	if w := instanceWire("plain-0"); !strings.Contains(w, `"asleep":true,"sleep_held":true,`) {
		t.Fatalf("plain-0 wire = %s", w)
	}
	if w := instanceWire("plain-1"); !strings.Contains(w, `"asleep":true,"sleep_held":false,`) {
		t.Fatalf("plain-1 wire = %s", w)
	}

	// A deployment-wide sleep holds both.
	if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=2", dep.ID); status != http.StatusOK {
		t.Fatalf("deployment sleep = %d %v", status, body)
	}
	if got := wire("deployment sleep", `"sleep_hold":"all","sleep_held":true`); got.SleepHold != dto.SleepHoldAll || !got.SleepHeld {
		t.Fatalf("all held = %q %v", got.SleepHold, got.SleepHeld)
	}

	// Waking plain-0 lifts its hold only.
	if rec := instanceRequest(f.h.wakeInstance, "plain-0", ""); rec.Code != http.StatusOK {
		t.Fatalf("wake plain-0 = %d %s", rec.Code, rec.Body)
	}
	if w := instanceWire("plain-0"); !strings.Contains(w, `"sleep_held":false`) {
		t.Fatalf("plain-0 after its wake = %s", w)
	}
	if w := instanceWire("plain-1"); !strings.Contains(w, `"asleep":true,"sleep_held":true,`) {
		t.Fatalf("plain-1 after plain-0's wake = %s", w)
	}
	wire("one held asleep, one awake", `"sleep_hold":"all","sleep_held":true`)

	// The arbiter's sleep of plain-0 again: a mix, and the hold of plain-1
	// stays.
	if err := f.st.OrderInstancesSleep(dep.ID, []string{"plain-0"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	wire("held + arbiter", `"sleep_hold":"some","sleep_held":false`)

	// Only the arbiter's sleepers: none held.
	if rec := instanceRequest(f.h.wakeInstance, "plain-1", ""); rec.Code != http.StatusOK {
		t.Fatalf("wake plain-1 = %d %s", rec.Code, rec.Body)
	}
	wire("arbiter only", `"sleep_hold":"none","sleep_held":false`)
}

// An operator's yielding wake holds exactly the replicas it puts to sleep:
// the co-tenant's replica on the card the wake needs, not its replica on
// another card. The waking sleeper's own hold is lifted as its wake waits.
func TestAYieldingWakeHoldsExactlyItsYielders(t *testing.T) {
	f := newWakeWaitFixture(t, true)
	gpu := store.GPU{Index: 1, UUID: "gpu-1", NodeID: f.node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 24 << 30}
	if err := f.st.DB().Create(&gpu).Error; err != nil {
		t.Fatal(err)
	}
	other := f.instance(t, f.newcomer, "newcomer-2", 0)
	if err := f.st.DB().Model(&store.Instance{}).Where("id = ?", other.ID).Update("GPUIndexes", "[1]").Error; err != nil {
		t.Fatal(err)
	}
	if in, _ := f.st.InstanceByUUID("sleeper-1"); !in.SleepHeld {
		t.Fatal("an instance started under a deployment-wide sleep is not held")
	}
	if status, body := f.deploymentCall(t, f.sleeper.ID, "?yield=true"); status != http.StatusAccepted {
		t.Fatalf("wake?yield=true = %d %s", status, body)
	}
	for uuid, want := range map[string]bool{"newcomer-1": true, "newcomer-2": false, "sleeper-1": false} {
		if in, _ := f.st.InstanceByUUID(uuid); in.SleepHeld != want {
			t.Errorf("%s held = %v, want %v", uuid, in.SleepHeld, want)
		}
	}
	if sleepIntent(t, f.st, "newcomer-2") != 0 {
		t.Fatal("the co-tenant's replica on another card was put to sleep")
	}
}

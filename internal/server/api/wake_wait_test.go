package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// wakeWaitFixture is a sleeping, sleepable deployment ("sleeper", one L1
// sleeper holding the only card) and an awake co-tenant ("newcomer") that has
// since taken the card.
type wakeWaitFixture struct {
	sleepFixture
	sleeper, newcomer *store.Deployment
}

func newWakeWaitFixture(t *testing.T, coTenantSleepable bool) wakeWaitFixture {
	t.Helper()
	f := newSleepFixture(t)
	sleeper := f.deployment(t, &store.Deployment{Name: "sleeper", ServedName: "sleeper", Engine: "vllm", Sleepable: true, DesiredSleep: 1})
	f.instance(t, sleeper, "sleeper-1", 1)
	newcomer := f.deployment(t, &store.Deployment{Name: "newcomer", ServedName: "newcomer", Engine: "vllm", Sleepable: coTenantSleepable})
	f.instance(t, newcomer, "newcomer-1", 0)
	return wakeWaitFixture{sleepFixture: f, sleeper: sleeper, newcomer: newcomer}
}

func (f wakeWaitFixture) waitingSince(t *testing.T, uuid string) *time.Time {
	t.Helper()
	inst, err := f.st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return inst.WakeWaitingSince
}

func (f wakeWaitFixture) holdOutside(t *testing.T) {
	t.Helper()
	if err := f.st.DB().Model(&store.GPU{}).Where("uuid = ?", "gpu-0").Update("foreign_processes", `[{"pid":1,"name":"python"}]`).Error; err != nil {
		t.Fatal(err)
	}
}

func (f wakeWaitFixture) deploymentCall(t *testing.T, id uint, query string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/x/wake"+query, nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": jsonOfUint(id)}))
	f.h.wakeDeployment(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestWakeWaitParksTheWakeWhenACoTenantHoldsTheCard(t *testing.T) {
	f := newWakeWaitFixture(t, false)

	status, body := f.deploymentCall(t, f.sleeper.ID, "?wait=true")
	const want = `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":[]}`
	if status != http.StatusAccepted || body != want {
		t.Fatalf("wake?wait=true = %d %s, want 202 %s", status, body, want)
	}
	if f.waitingSince(t, "sleeper-1") == nil {
		t.Fatal("the instance row has no wake_waiting_since")
	}
	// Waiting is not a wake: the intent stays asleep, and the co-tenant is untouched.
	if got, _ := f.st.GetDeployment(f.sleeper.ID); got.DesiredSleep != 1 {
		t.Fatalf("waiting changed the sleeper's desired level to %d", got.DesiredSleep)
	}
	if sleepIntent(t, f.st, "sleeper-1") != 1 || sleepIntent(t, f.st, "newcomer-1") != 0 {
		t.Fatal("waiting changed a sleep intent")
	}
	events, err := f.st.DeploymentEvents(f.sleeper.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var recorded bool
	for _, ev := range events {
		recorded = recorded || (ev.Kind == "automation" && ev.Code == "WAKE_WAITING" && strings.Contains(ev.Data, `"trigger":"operator"`))
	}
	if !recorded {
		t.Fatalf("no WAKE_WAITING automation event: %+v", events)
	}

	// The deployment on the wire says since when.
	wire, _ := json.Marshal(f.h.deploymentDTO(f.sleeper, true))
	if !strings.Contains(string(wire), `"wake_waiting_since":"`) {
		t.Fatalf("deployment wire lacks wake_waiting_since: %s", wire)
	}
	other, _ := json.Marshal(f.h.deploymentDTO(f.newcomer, true))
	if strings.Contains(string(other), "wake_waiting_since") {
		t.Fatalf("a deployment with no waiting wake carries wake_waiting_since: %s", other)
	}

	// Without ?wait the same wake is still refused outright, and ?wait=false is that.
	for _, query := range []string{"", "?wait=false", "?yield=false"} {
		status, body := f.deploymentCall(t, f.sleeper.ID, query)
		if code, _ := errorBody(t, body); status != http.StatusConflict || code != "WAKE_INSUFFICIENT_GPU_MEMORY" {
			t.Fatalf("wake%s = %d %s", query, status, body)
		}
	}
}

func TestEndWakeWaitingWithdrawsTheWait(t *testing.T) {
	f := newWakeWaitFixture(t, false)
	if status, body := f.deploymentCall(t, f.sleeper.ID, "?wait=true"); status != http.StatusAccepted {
		t.Fatalf("wait = %d %s", status, body)
	}
	call := func(id uint) (int, string) {
		status, body := f.call(t, f.h.endWakeWaiting, "/api/v1/deployments/x/wake-waiting", id)
		raw, _ := json.Marshal(body)
		return status, string(raw)
	}
	status, body := call(f.sleeper.ID)
	if status != http.StatusOK || body != `{"items":["sleeper-1"],"total":1}` {
		t.Fatalf("end waiting = %d %s", status, body)
	}
	if f.waitingSince(t, "sleeper-1") != nil {
		t.Fatal("wake_waiting_since was not cleared")
	}
	if got, _ := f.st.GetDeployment(f.sleeper.ID); got.DesiredSleep != 1 || sleepIntent(t, f.st, "sleeper-1") != 1 {
		t.Fatal("withdrawing the wait woke the sleeper")
	}
	if status, body = call(f.sleeper.ID); status != http.StatusOK || body != `{"items":[],"total":0}` {
		t.Fatalf("second end = %d %s, want an empty list", status, body)
	}
	if status, _ = call(9999); status != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", status)
	}
}

func TestWakeWaitWakesAtOnceWhenTheCardIsFree(t *testing.T) {
	f := newWakeWaitFixture(t, false)
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "newcomer-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	status, body := f.deploymentCall(t, f.sleeper.ID, "?wait=true")
	if want := `{"status":"waking","woken":["sleeper-1"],"waiting":[],"yielded":[]}`; status != http.StatusOK || body != want {
		t.Fatalf("wake?wait=true = %d %s, want 200 %s", status, body, want)
	}
	if f.waitingSince(t, "sleeper-1") != nil {
		t.Fatal("a wake that fit is marked waiting")
	}
	if got, _ := f.st.GetDeployment(f.sleeper.ID); got.DesiredSleep != 0 || sleepIntent(t, f.st, "sleeper-1") != 0 {
		t.Fatalf("the sleeper was not woken: desired %d", got.DesiredSleep)
	}
}

func TestWakeYieldPutsASleepableCoTenantToSleep(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		level       int
	}{
		{"default level 1", "?yield=true", 1},
		{"level 2", "?yield=true&yield_level=2", 2},
		{"yield with wait", "?yield=true&wait=true&yield_level=1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWakeWaitFixture(t, true)
			status, body := f.deploymentCall(t, f.sleeper.ID, tc.query)
			const want = `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["newcomer"]}`
			if status != http.StatusAccepted || body != want {
				t.Fatalf("wake%s = %d %s, want 202 %s", tc.query, status, body, want)
			}
			if got := sleepIntent(t, f.st, "newcomer-1"); got != tc.level {
				t.Fatalf("co-tenant's desired_sleep = %d, want %d", got, tc.level)
			}
			inst, _ := f.st.InstanceByUUID("newcomer-1")
			if !strings.Contains(inst.DesiredStateJSON, `"sleep_mode":"wait"`) {
				t.Fatalf("the yield did not use the waiting pause mode: %s", inst.DesiredStateJSON)
			}
			// The waking instance waits for the card to come free; it is not woken yet.
			if f.waitingSince(t, "sleeper-1") == nil || sleepIntent(t, f.st, "sleeper-1") != 1 {
				t.Fatalf("the sleeper must be waiting and still asleep")
			}
			if f.waitingSince(t, "newcomer-1") != nil {
				t.Fatal("the yielding co-tenant is marked waiting")
			}
			events, err := f.st.DeploymentEvents(f.newcomer.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			var recorded bool
			for _, ev := range events {
				recorded = recorded || (ev.Kind == "automation" && ev.Code == "YIELDED_FOR_WAKE" && strings.Contains(ev.Data, `"deployment":"sleeper"`))
			}
			if !recorded {
				t.Fatalf("the co-tenant has no YIELDED_FOR_WAKE event: %+v", events)
			}
			// The co-tenant's deployment-wide level is not an order of its own.
			got, _ := f.st.GetDeployment(f.newcomer.ID)
			if got.DesiredSleep != 0 {
				t.Fatalf("a replica-level yield set the deployment level to %d", got.DesiredSleep)
			}
			// The yielder stays asleep, held: a request does not wake it
			// (store.Deployment.WakesOnRequest, which the gateway and the
			// residency arbiter read); only an operator's or a schedule's
			// wake does (docs/explanation/service-levels.md).
			if !inst.SleepHeld || got.WakesOnRequest(inst) {
				t.Fatalf("the yielder is held=%v wakes on request=%v, want held and not woken by a request", inst.SleepHeld, got.WakesOnRequest(inst))
			}
		})
	}
}

func TestWakeYieldIsBlockedByWhatCannotSleep(t *testing.T) {
	t.Run("a co-tenant without sleep mode", func(t *testing.T) {
		f := newWakeWaitFixture(t, false)
		status, body := f.deploymentCall(t, f.sleeper.ID, "?yield=true")
		code, params := errorBody(t, body)
		if status != http.StatusConflict || code != "WAKE_YIELD_BLOCKED" {
			t.Fatalf("yield = %d %s, want 409 WAKE_YIELD_BLOCKED", status, body)
		}
		if got, _ := json.Marshal(params["occupants"]); string(got) != `["newcomer"]` {
			t.Fatalf("params = %v", params)
		}
		if got, _ := json.Marshal(params["woken"]); string(got) != `[]` {
			t.Fatalf("params.woken = %s, want []", got)
		}
		if sleepIntent(t, f.st, "newcomer-1") != 0 || f.waitingSince(t, "sleeper-1") != nil || sleepIntent(t, f.st, "sleeper-1") != 1 {
			t.Fatal("a blocked yield changed something")
		}
		if got, _ := f.st.GetDeployment(f.sleeper.ID); got.DesiredSleep != 1 {
			t.Fatalf("a blocked yield changed the sleeper's level to %d", got.DesiredSleep)
		}
	})

	t.Run("a process outside the platform", func(t *testing.T) {
		f := newWakeWaitFixture(t, true)
		f.holdOutside(t)
		status, body := f.deploymentCall(t, f.sleeper.ID, "?yield=true")
		code, params := errorBody(t, body)
		if status != http.StatusConflict || code != "WAKE_YIELD_BLOCKED" {
			t.Fatalf("yield = %d %s", status, body)
		}
		if got, _ := json.Marshal(params["occupants"]); string(got) != `["process outside the platform"]` {
			t.Fatalf("occupants = %s", got)
		}
		// Putting only the platform's occupant to sleep would not make room.
		if sleepIntent(t, f.st, "newcomer-1") != 0 || f.waitingSince(t, "sleeper-1") != nil {
			t.Fatal("a blocked yield changed something")
		}
	})
}

func TestWakeWaitParameterValidation(t *testing.T) {
	f := newWakeWaitFixture(t, true)
	for _, query := range []string{"?wait=maybe", "?yield=2x", "?wait=true&yield=nope"} {
		status, body := f.deploymentCall(t, f.sleeper.ID, query)
		if status != http.StatusBadRequest || !strings.Contains(body, `"error"`) {
			t.Fatalf("wake%s = %d %s, want 400", query, status, body)
		}
	}
	for _, query := range []string{"?yield=true&yield_level=3", "?yield=true&yield_level=0", "?wait=true&yield_level=x"} {
		status, body := f.deploymentCall(t, f.sleeper.ID, query)
		code, params := errorBody(t, body)
		if status != http.StatusBadRequest || code != "SLEEP_INVALID_LEVEL" || params["level"] == "" {
			t.Fatalf("wake%s = %d %s, want 400 SLEEP_INVALID_LEVEL", query, status, body)
		}
	}
	if sleepIntent(t, f.st, "newcomer-1") != 0 || f.waitingSince(t, "sleeper-1") != nil || sleepIntent(t, f.st, "sleeper-1") != 1 {
		t.Fatal("a refused request changed something")
	}
	// The parameters are parsed after the deployment is resolved and checked.
	if status, _ := f.deploymentCall(t, 9999, "?wait=true"); status != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", status)
	}
	plain := f.deployment(t, &store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm"})
	status, body := f.deploymentCall(t, plain.ID, "?wait=true")
	if code, _ := errorBody(t, body); status != http.StatusBadRequest || code != "SLEEP_NOT_ENABLED" {
		t.Fatalf("a deployment without sleep mode: %d %s", status, body)
	}
}

func TestWakeInstanceWaitAndYield(t *testing.T) {
	t.Run("wait", func(t *testing.T) {
		f := newWakeWaitFixture(t, false)
		rec := instanceRequest(f.h.wakeInstance, "sleeper-1", "?wait=true")
		const want = `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":[]}`
		if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("wake?wait=true = %d %s", rec.Code, rec.Body)
		}
		if f.waitingSince(t, "sleeper-1") == nil || sleepIntent(t, f.st, "sleeper-1") != 1 {
			t.Fatal("the instance must wait and stay asleep")
		}
		if rec = instanceRequest(f.h.wakeInstance, "sleeper-1", "?wait=maybe"); rec.Code != http.StatusBadRequest {
			t.Fatalf("wait=maybe = %d %s", rec.Code, rec.Body)
		}
		rec = instanceRequest(f.h.wakeInstance, "sleeper-1", "?yield_level=3")
		if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusBadRequest || code != "SLEEP_INVALID_LEVEL" {
			t.Fatalf("yield_level=3 = %d %s", rec.Code, rec.Body)
		}
		if rec = instanceRequest(f.h.wakeInstance, "no-such-instance", "?wait=true"); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown instance = %d", rec.Code)
		}
	})
	t.Run("yield", func(t *testing.T) {
		f := newWakeWaitFixture(t, true)
		rec := instanceRequest(f.h.wakeInstance, "sleeper-1", "?yield=true&yield_level=2")
		const want = `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["newcomer"]}`
		if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("wake?yield=true = %d %s", rec.Code, rec.Body)
		}
		if sleepIntent(t, f.st, "newcomer-1") != 2 || f.waitingSince(t, "sleeper-1") == nil {
			t.Fatal("the co-tenant must be put to sleep at level 2 and the sleeper wait")
		}
	})
	t.Run("yield blocked", func(t *testing.T) {
		f := newWakeWaitFixture(t, false)
		rec := instanceRequest(f.h.wakeInstance, "sleeper-1", "?yield=true")
		if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusConflict || code != "WAKE_YIELD_BLOCKED" {
			t.Fatalf("wake?yield=true = %d %s", rec.Code, rec.Body)
		}
		if sleepIntent(t, f.st, "newcomer-1") != 0 || f.waitingSince(t, "sleeper-1") != nil {
			t.Fatal("a blocked yield changed something")
		}
	})
	t.Run("an instance of a deployment without sleep mode", func(t *testing.T) {
		f := newWakeWaitFixture(t, true)
		// An instance whose deployment cannot sleep has nothing to wait for.
		plain := f.deployment(t, &store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm"})
		f.instance(t, plain, "plain-1", 0)
		rec := instanceRequest(f.h.wakeInstance, "plain-1", "?wait=true")
		if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusBadRequest || code != "SLEEP_NOT_ENABLED" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}

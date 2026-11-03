package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// sharing gives dep a yield policy other than never, as an operator does.
func (f sleepFixture) sharing(t *testing.T, dep *store.Deployment, policy string) *store.Deployment {
	t.Helper()
	if err := f.st.SetDeploymentYieldPolicy(dep.ID, yieldpolicy.Defaults(policy)); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func heldOf(t *testing.T, st *store.Store, uuid string) bool {
	t.Helper()
	in, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return in.SleepHeld
}

// An operator's sleep is held unless the order says a request wakes it; with
// nothing said, a deployment that shares its cards by yield policy is woken by
// a request and one that never yields is held. A sleep a request wakes names
// the awake replicas rather than putting the deployment as a whole under the
// order. The deployment says its default on the wire, the order its choice,
// and the timeline the choice.
func TestOperatorSleepWakesOnRequestByPolicyOrOrder(t *testing.T) {
	for _, tc := range []struct {
		name, policy, query string
		wakes               bool
	}{
		{"never yields: held", yieldpolicy.Never, "", false},
		{"timeslice: a request wakes it", yieldpolicy.TimeSlice, "", true},
		{"idle: a request wakes it", yieldpolicy.Idle, "", true},
		{"timeslice, held by the order", yieldpolicy.TimeSlice, "&wake_on_request=false", false},
		{"never yields, woken by request by the order", yieldpolicy.Never, "&wake_on_request=true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSleepFixture(t)
			dep := f.deployment(t, &store.Deployment{Name: "d", ServedName: "d", Engine: "vllm", Sleepable: true})
			f.instance(t, dep, "d-0", 0)
			if tc.policy != yieldpolicy.Never {
				dep = f.sharing(t, dep, tc.policy)
			}
			wantDefault := tc.policy != yieldpolicy.Never
			if body := f.deploymentWireOf(t, dep.ID); !strings.Contains(body, `"sleep_wakes_on_request":`+map[bool]string{true: "true", false: "false"}[wantDefault]) {
				t.Fatalf("deployment wire lacks its default %v: %s", wantDefault, body)
			}
			status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1"+tc.query, dep.ID)
			if status != http.StatusOK || body["wake_on_request"] != tc.wakes || body["status"] != "sleeping" {
				t.Fatalf("sleep = %d %v", status, body)
			}
			if sleepIntent(t, f.st, "d-0") != 1 {
				t.Fatal("the replica was not ordered to sleep")
			}
			got, _ := f.st.GetDeployment(dep.ID)
			in, _ := f.st.InstanceByUUID("d-0")
			if in.SleepHeld == tc.wakes || got.WakesOnRequest(in) != tc.wakes {
				t.Fatalf("held=%v wakes on request=%v, want wakes %v", in.SleepHeld, got.WakesOnRequest(in), tc.wakes)
			}
			// Only a held sleep puts the deployment as a whole under the order.
			if want := map[bool]int{true: 0, false: 1}[tc.wakes]; got.DesiredSleep != want {
				t.Fatalf("deployment desired_sleep = %d, want %d", got.DesiredSleep, want)
			}
			events, _ := f.st.DeploymentEvents(dep.ID, 10)
			var recorded bool
			for _, ev := range events {
				recorded = recorded || ev.Code == store.EventSleepOrdered && strings.Contains(ev.Data, `"wake_on_request":`+map[bool]string{true: "true", false: "false"}[tc.wakes])
			}
			if !recorded {
				t.Fatalf("no SLEEP_ORDERED with the choice: %+v", events)
			}
		})
	}
}

// A replica's sleep takes the same choice; a value that is not a boolean is
// refused before anything is written.
func TestReplicaSleepWakeOnRequestAndValidation(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	f.sharing(t, dep, yieldpolicy.Idle)
	if rec := instanceRequest(f.h.sleepInstance, "plain-0", "?level=1"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"wake_on_request":true`) {
		t.Fatalf("sleep plain-0 = %d %s", rec.Code, rec.Body)
	}
	if heldOf(t, f.st, "plain-0") {
		t.Fatal("an idle-policy replica's sleep is held by default")
	}
	if rec := instanceRequest(f.h.sleepInstance, "plain-1", "?level=1&wake_on_request=false"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"wake_on_request":false`) {
		t.Fatalf("sleep plain-1 held = %d %s", rec.Code, rec.Body)
	}
	if !heldOf(t, f.st, "plain-1") {
		t.Fatal("a sleep the order holds is not held")
	}
	for _, handler := range []struct {
		name string
		run  func() (int, string)
	}{
		{"replica", func() (int, string) {
			rec := instanceRequest(f.h.sleepInstance, "plain-0", "?wake_on_request=maybe")
			return rec.Code, rec.Body.String()
		}},
		{"deployment", func() (int, string) {
			status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?wake_on_request=maybe", dep.ID)
			raw, _ := json.Marshal(body)
			return status, string(raw)
		}},
	} {
		status, body := handler.run()
		if status != http.StatusBadRequest || !strings.Contains(body, `"code":"SLEEP_INVALID_WAKE_ON_REQUEST"`) || !strings.Contains(body, `"wake_on_request":"maybe"`) {
			t.Fatalf("%s: invalid wake_on_request = %d %s", handler.name, status, body)
		}
	}
}

// An operator's yielding wake puts the occupant to sleep as its own policy
// says: one that shares its cards is woken by a request again and is not in
// yielded_held; one that never yields is held and is.
func TestAYieldingWakeHoldsOnlyOccupantsThatNeverYield(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		held         bool
		want         string
	}{
		{"never", yieldpolicy.Never, true, `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["newcomer"],"yielded_held":["newcomer"]}`},
		{"timeslice", yieldpolicy.TimeSlice, false, `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["newcomer"],"yielded_held":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWakeWaitFixture(t, true)
			if tc.policy != yieldpolicy.Never {
				f.sharing(t, f.newcomer, tc.policy)
			}
			status, body := f.deploymentCall(t, f.sleeper.ID, "?yield=true")
			if status != http.StatusAccepted || body != tc.want {
				t.Fatalf("wake?yield=true = %d %s, want %s", status, body, tc.want)
			}
			if sleepIntent(t, f.st, "newcomer-1") != 1 || heldOf(t, f.st, "newcomer-1") != tc.held {
				t.Fatalf("occupant intent %d held %v, want asleep held=%v", sleepIntent(t, f.st, "newcomer-1"), heldOf(t, f.st, "newcomer-1"), tc.held)
			}
			events, _ := f.st.DeploymentEvents(f.newcomer.ID, 10)
			var recorded bool
			for _, ev := range events {
				recorded = recorded || ev.Code == "YIELDED_FOR_WAKE" && strings.Contains(ev.Data, `"wake_on_request":`+map[bool]string{true: "false", false: "true"}[tc.held])
			}
			if !recorded {
				t.Fatalf("no YIELDED_FOR_WAKE with the choice: %+v", events)
			}
		})
	}
}

// Letting requests wake a held deployment lifts every hold without waking
// anything, withdraws the deployment-wide order so a new replica does not
// start held, records who did it, and is idempotent; a deployment that cannot
// sleep is refused.
func TestReleaseSleepHold(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=2", dep.ID); status != http.StatusOK || body["wake_on_request"] != false {
		t.Fatalf("held sleep = %d %v", status, body)
	}
	if !heldOf(t, f.st, "plain-0") || !heldOf(t, f.st, "plain-1") {
		t.Fatal("the deployment-wide sleep holds nothing")
	}
	status, body := f.call(t, f.h.releaseSleepHold, "/api/v1/deployments/x/wake-on-request", dep.ID)
	if raw, _ := json.Marshal(body); status != http.StatusOK || string(raw) != `{"released":["plain-0","plain-1"]}` {
		t.Fatalf("release = %d %s", status, raw)
	}
	got, _ := f.st.GetDeployment(dep.ID)
	if got.DesiredSleep != 0 {
		t.Fatalf("the deployment-wide order stays at %d", got.DesiredSleep)
	}
	for _, uuid := range []string{"plain-0", "plain-1"} {
		in, _ := f.st.InstanceByUUID(uuid)
		if in.SleepHeld || !got.WakesOnRequest(in) || sleepIntent(t, f.st, uuid) != 2 {
			t.Fatalf("%s: held=%v wakes=%v intent=%d, want asleep at 2 and woken by a request", uuid, in.SleepHeld, got.WakesOnRequest(in), sleepIntent(t, f.st, uuid))
		}
	}
	if body := f.deploymentWireOf(t, dep.ID); !strings.Contains(body, `"sleep_hold":"none","sleep_held":false`) {
		t.Fatalf("deployment wire after the release = %s", body)
	}
	events, _ := f.st.DeploymentEvents(dep.ID, 10)
	var recorded bool
	for _, ev := range events {
		recorded = recorded || ev.Code == store.EventSleepHoldReleased && strings.Contains(ev.Data, `"instances":["plain-0","plain-1"]`)
	}
	if !recorded {
		t.Fatalf("no SLEEP_HOLD_RELEASED: %+v", events)
	}
	if kind, ok := store.SchedulingEventKind(store.EventSleepHoldReleased); !ok || kind != store.EventKindLifecycle {
		t.Fatalf("the history does not list the release: %q %v", kind, ok)
	}
	// Again: nothing is held, nothing is recorded.
	before := len(events)
	status, body = f.call(t, f.h.releaseSleepHold, "/api/v1/deployments/x/wake-on-request", dep.ID)
	if raw, _ := json.Marshal(body); status != http.StatusOK || string(raw) != `{"released":[]}` {
		t.Fatalf("second release = %d %s", status, raw)
	}
	if after, _ := f.st.DeploymentEvents(dep.ID, 10); len(after) != before {
		t.Fatalf("an empty release was recorded: %d events, was %d", len(after), before)
	}

	plain := f.deployment(t, &store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm"})
	if status, body := f.call(t, f.h.releaseSleepHold, "/api/v1/deployments/x/wake-on-request", plain.ID); status != http.StatusBadRequest || body["code"] != ErrorSleepNotEnabled {
		t.Fatalf("a deployment without sleep = %d %v", status, body)
	}
	if status, _ := f.call(t, f.h.releaseSleepHold, "/api/v1/deployments/x/wake-on-request", 999); status != http.StatusNotFound {
		t.Fatalf("an unknown deployment = %d", status)
	}
}

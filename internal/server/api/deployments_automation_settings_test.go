package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentWire reads the deployment as the API serves it.
func (r *scheduleRig) deploymentWire(t *testing.T, dep *store.Deployment) map[string]any {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", dep.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deployment = %d %s", rec.Code, rec.Body)
	}
	return decodeBody(t, rec)
}

func TestScheduleRoutesRequireAuthAndFindTheirTarget(t *testing.T) {
	rig := newScheduleRig(t)
	own := &store.LifecycleSchedule{DeploymentID: rig.sleepable.ID, Action: store.ScheduleActionWake, Weekdays: 1 << 1, Minute: 8 * 60, Enabled: true}
	other := &store.LifecycleSchedule{DeploymentID: rig.plain.ID, Action: store.ScheduleActionWake, Weekdays: 1 << 1, Minute: 9 * 60, Enabled: true}
	for _, row := range []*store.LifecycleSchedule{own, other} {
		if err := rig.st.CreateLifecycleSchedule(row); err != nil {
			t.Fatal(err)
		}
	}
	const wakeBody = `{"action":"wake","weekdays":[1],"time":"07:00"}`
	routes := []struct {
		method, tail, body string
	}{
		{http.MethodGet, "schedule", ""},
		{http.MethodPost, "schedule", wakeBody},
		{http.MethodPut, fmt.Sprintf("schedule/%d", own.ID), wakeBody},
		{http.MethodDelete, fmt.Sprintf("schedule/%d", own.ID), ""},
		{http.MethodPut, "idle-sleep", `{"after_sec":900,"level":2}`},
		{http.MethodPut, "node-replacement", `{"after_sec":600}`},
		{http.MethodDelete, "wake-waiting", ""},
	}

	t.Run("no token is 401 and changes nothing", func(t *testing.T) {
		for _, route := range routes {
			rec := rig.do("", route.method, schedPath(rig.sleepable, route.tail), route.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d %s, want 401", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		if rows, _ := rig.st.LifecycleSchedules(rig.sleepable.ID); len(rows) != 1 {
			t.Fatalf("an unauthenticated request changed the schedule: %+v", rows)
		}
		if _, err := rig.st.LifecycleSchedule(rig.sleepable.ID, own.ID); err != nil {
			t.Fatalf("unauthenticated delete removed the order: %v", err)
		}
	})

	t.Run("a read token reads but cannot change", func(t *testing.T) {
		read := rig.mint("read")
		for _, route := range routes {
			rec := rig.do(read, route.method, schedPath(rig.sleepable, route.tail), route.body)
			if route.method == http.MethodGet {
				if rec.Code != http.StatusOK {
					t.Fatalf("read token GET %s = %d %s", route.tail, rec.Code, rec.Body)
				}
				continue
			}
			if rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
				t.Fatalf("read token %s %s = %d %s, want 403 PLATFORM_TOKEN_READ_ONLY", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		if rows, _ := rig.st.LifecycleSchedules(rig.sleepable.ID); len(rows) != 1 || rows[0].Minute != 8*60 {
			t.Fatalf("a refused read-token request changed the schedule: %+v", rows)
		}
		if dep, _ := rig.st.GetDeployment(rig.sleepable.ID); dep.IdleSleepAfterSec != 0 || dep.NodeReplaceAfterSec != 600 {
			t.Fatalf("a refused read-token request changed the settings: idle=%d replace=%d", dep.IdleSleepAfterSec, dep.NodeReplaceAfterSec)
		}
	})

	t.Run("a write token manages the schedule", func(t *testing.T) {
		write := rig.mint("write")
		rec := rig.do(write, http.MethodPost, schedPath(rig.sleepable, "schedule"), wakeBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("write token POST = %d %s", rec.Code, rec.Body)
		}
		if rec = rig.do(write, http.MethodPut, schedPath(rig.sleepable, "idle-sleep"), `{"after_sec":0,"level":2}`); rec.Code != http.StatusOK {
			t.Fatalf("write token PUT idle-sleep = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown deployment or order is 404", func(t *testing.T) {
		for _, route := range routes {
			rec := rig.do(rig.session, route.method, "/api/v1/deployments/9999/"+route.tail, route.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unknown deployment: %s %s = %d %s", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := rig.do(rig.session, method, schedPath(rig.sleepable, "schedule/9999"), wakeBody)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unknown order: %s = %d %s", method, rec.Code, rec.Body)
			}
			// An order of another deployment is not this deployment's.
			rec = rig.do(rig.session, method, schedPath(rig.sleepable, fmt.Sprintf("schedule/%d", other.ID)), wakeBody)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("another deployment's order: %s = %d %s", method, rec.Code, rec.Body)
			}
		}
		if got, err := rig.st.LifecycleSchedule(rig.plain.ID, other.ID); err != nil || got.Minute != 9*60 || got.Action != store.ScheduleActionWake {
			t.Fatalf("another deployment's order was touched: %+v %v", got, err)
		}
		if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepable, "schedule/abc"), wakeBody); rec.Code != http.StatusNotFound {
			t.Fatalf("non-numeric order id = %d %s", rec.Code, rec.Body)
		}
		// A listing never shows another deployment's orders.
		rec := rig.do(rig.session, http.MethodGet, schedPath(rig.plain, "schedule"), "")
		if body := decodeBody(t, rec); body["total"] != float64(1) {
			t.Fatalf("plain deployment's list = %s", rec.Body)
		}
	})

	t.Run("a method a route does not take is 405", func(t *testing.T) {
		for _, tc := range []struct{ method, tail string }{
			{http.MethodGet, fmt.Sprintf("schedule/%d", own.ID)},
			{http.MethodPost, fmt.Sprintf("schedule/%d", own.ID)},
			{http.MethodPut, "schedule"},
			{http.MethodDelete, "schedule"},
			{http.MethodGet, "idle-sleep"},
			{http.MethodPost, "idle-sleep"},
			{http.MethodGet, "node-replacement"},
			{http.MethodPost, "node-replacement"},
			{http.MethodGet, "wake-waiting"},
			{http.MethodPut, "wake-waiting"},
		} {
			for _, token := range []string{rig.session, rig.mint("read")} {
				// Routing answers before the scope check, for either token.
				want := http.StatusMethodNotAllowed
				if rec := rig.do(token, tc.method, schedPath(rig.sleepable, tc.tail), "{}"); rec.Code != want {
					t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.tail, rec.Code, rec.Body, want)
				}
			}
		}
	})
}

func TestIdleSleepSetting(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.sleepable, "idle-sleep")

	if got := rig.deploymentWire(t, rig.sleepable)["idle_sleep"]; jsonOf(t, got) != `{"after_sec":0,"level":2}` {
		t.Fatalf("a new deployment's idle_sleep = %s, want off at level 2", jsonOf(t, got))
	}

	rec := rig.do(rig.session, http.MethodPut, path, `{"after_sec":900,"level":2}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"after_sec":900,"level":2}` {
		t.Fatalf("set = %d %s", rec.Code, rec.Body)
	}
	if got := rig.deploymentWire(t, rig.sleepable)["idle_sleep"]; jsonOf(t, got) != `{"after_sec":900,"level":2}` {
		t.Fatalf("deployment idle_sleep = %s", jsonOf(t, got))
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepable.ID); dep.IdleSleepAfterSec != 900 || dep.IdleSleepLevel != 2 {
		t.Fatalf("stored = %d/%d", dep.IdleSleepAfterSec, dep.IdleSleepLevel)
	}

	// Bounds: 60 s to 7 days, or 0 for off.
	for _, ok := range []string{`{"after_sec":60,"level":1}`, `{"after_sec":604800,"level":2}`, `{"after_sec":0,"level":1}`} {
		if rec := rig.do(rig.session, http.MethodPut, path, ok); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != ok {
			t.Fatalf("PUT %s = %d %s", ok, rec.Code, rec.Body)
		}
	}
	if got := rig.deploymentWire(t, rig.sleepable)["idle_sleep"]; jsonOf(t, got) != `{"after_sec":0,"level":1}` {
		t.Fatalf("after turning off, idle_sleep = %s", jsonOf(t, got))
	}
	if rec := rig.do(rig.session, http.MethodPut, path, `{"after_sec":900,"level":2}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}

	for _, tc := range []struct{ name, body, field string }{
		{"below the minimum", `{"after_sec":30,"level":2}`, "after_sec"},
		{"just below the minimum", `{"after_sec":59,"level":2}`, "after_sec"},
		{"negative", `{"after_sec":-1,"level":2}`, "after_sec"},
		{"above 7 days", `{"after_sec":604801,"level":2}`, "after_sec"},
		{"level 3", `{"after_sec":900,"level":3}`, "level"},
		{"level 0", `{"after_sec":900,"level":0}`, "level"},
		{"level missing", `{"after_sec":900}`, "level"},
		{"off with a bad level", `{"after_sec":0,"level":5}`, "level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.do(rig.session, http.MethodPut, path, tc.body)
			body := decodeBody(t, rec)
			params, _ := body["params"].(map[string]any)
			if rec.Code != http.StatusBadRequest || body["code"] != "IDLE_SLEEP_INVALID" || params["field"] != tc.field {
				t.Fatalf("PUT %s = %d %s, want 400 IDLE_SLEEP_INVALID field %s", tc.body, rec.Code, rec.Body, tc.field)
			}
			if tc.field == "after_sec" && (params["minimum"] != float64(60) || params["maximum"] != float64(604800)) {
				t.Fatalf("params = %v, want the bounds", params)
			}
		})
	}
	// Turning idle sleep off need not restate the level: the stored one is
	// kept for when it is turned on again.
	if rec := rig.do(rig.session, http.MethodPut, path, `{"after_sec":0}`); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"after_sec":0,"level":2}` {
		t.Fatalf("PUT off without a level = %d %s, want 200 keeping level 2", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"after_sec":900,"level":2,"force":true}`, `{"after_sec":"900","level":2}`, `{"after_sec":`, ``} {
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepable.ID); dep.IdleSleepAfterSec != 0 || dep.IdleSleepLevel != 2 {
		t.Fatalf("a refused request changed the setting: %d/%d, want the 0/2 the last accepted one wrote", dep.IdleSleepAfterSec, dep.IdleSleepLevel)
	}

	t.Run("a deployment that cannot sleep", func(t *testing.T) {
		plainPath := schedPath(rig.plain, "idle-sleep")
		rec := rig.do(rig.session, http.MethodPut, plainPath, `{"after_sec":900,"level":2}`)
		body := decodeBody(t, rec)
		if rec.Code != http.StatusBadRequest || body["code"] != "SLEEP_NOT_ENABLED" {
			t.Fatalf("turn on = %d %s", rec.Code, rec.Body)
		}
		if dep, _ := rig.st.GetDeployment(rig.plain.ID); dep.IdleSleepAfterSec != 0 {
			t.Fatalf("a refused request set idle sleep to %d", dep.IdleSleepAfterSec)
		}
		// Turning it off is always allowed.
		rec = rig.do(rig.session, http.MethodPut, plainPath, `{"after_sec":0,"level":2}`)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"after_sec":0,"level":2}` {
			t.Fatalf("turn off = %d %s", rec.Code, rec.Body)
		}
	})
}

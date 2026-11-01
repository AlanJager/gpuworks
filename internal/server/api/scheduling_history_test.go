package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (r *scheduleRig) history(t *testing.T, bearer, query string) (int, []map[string]any, string) {
	t.Helper()
	rec := r.do(bearer, http.MethodGet, "/api/v1/scheduling/history"+query, "")
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %s: %v", rec.Body, err)
		}
	}
	return rec.Code, out.Items, rec.Body.String()
}

func historyCodesOf(items []map[string]any) string {
	codes := make([]string, 0, len(items))
	for _, item := range items {
		codes = append(codes, fmt.Sprint(item["code"]))
	}
	return strings.Join(codes, ",")
}

func TestSchedulingHistoryWire(t *testing.T) {
	rig := newScheduleRig(t)
	rig.st.AddDeploymentEventCode(rig.sleepable.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "idle", "level": 2, "idle_sec": 900})
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindLifecycle, store.EventReplicasChanged, map[string]any{"from": 1, "to": 3, "actor": "alice"})
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindSchedule, store.EventDeploymentEdited, map[string]any{"fields": "name"}) // not scheduling
	rig.st.AddDeploymentEventCode(rig.multi.ID, store.EventKindLifecycle, store.EventReplicaRebuilt, map[string]any{"replica": 1, "attempt": 2, "reason": "oom", "node": "gpu-1"})

	status, items, body := rig.history(t, rig.session, "")
	if status != http.StatusOK || historyCodesOf(items) != "REPLICA_REBUILT,REPLICAS_CHANGED,AUTO_SLEEP" {
		t.Fatalf("history = %d %s", status, body)
	}
	// The wire form of each entry, exactly: id and time vary, the rest is fixed.
	want := []string{
		`{"category":"failure","code":"REPLICA_REBUILT","deployment":"multi","deployment_id":%d,"kind":"lifecycle","nodes":[{"name":"gpu-1"}],"params":{"attempt":2,"node":"gpu-1","reason":"oom","replica":1}}`,
		`{"actor":"alice","category":"manual","code":"REPLICAS_CHANGED","deployment":"plain","deployment_id":%d,"kind":"lifecycle","params":{"actor":"alice","from":1,"to":3}}`,
		`{"category":"auto","code":"AUTO_SLEEP","deployment":"sleepy","deployment_id":%d,"kind":"automation","params":{"idle_sec":900,"level":2,"trigger":"idle"}}`,
	}
	ids := []uint{rig.multi.ID, rig.plain.ID, rig.sleepable.ID}
	for i, item := range items {
		if id, ok := item["id"].(float64); !ok || id < 1 {
			t.Fatalf("item %d has no id: %v", i, item)
		}
		at, ok := item["created_at"].(string)
		if !ok || !strings.HasSuffix(at, "Z") {
			t.Fatalf("item %d created_at = %v, want a UTC time", i, item["created_at"])
		}
		delete(item, "id")
		delete(item, "created_at")
		got, _ := json.Marshal(item)
		if wantItem := fmt.Sprintf(want[i], ids[i]); string(got) != wantItem {
			t.Errorf("item %d\n got %s\nwant %s", i, got, wantItem)
		}
	}
}

func TestSchedulingHistoryFiltersAndCursor(t *testing.T) {
	rig := newScheduleRig(t)
	add := func(dep *store.Deployment, kind, code string, params map[string]any) {
		rig.st.AddDeploymentEventCode(dep.ID, kind, code, params)
	}
	add(rig.sleepable, store.EventKindAutomation, "AUTO_WAKE", map[string]any{"trigger": "schedule"})
	add(rig.plain, store.EventKindSchedule, store.EventUnitRetried, map[string]any{})
	add(rig.plain, store.EventKindAutomation, "NODE_REPLACED", map[string]any{"from": "gpu-1"})
	add(rig.sleepable, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{})

	for query, want := range map[string]string{
		"?category=manual":                                       "UNIT_RETRIED",
		"?category=auto,failure":                                 "DEPLOYMENT_UNSCHEDULABLE,NODE_REPLACED,AUTO_WAKE",
		"?category=failure&category=manual":                      "DEPLOYMENT_UNSCHEDULABLE,NODE_REPLACED,UNIT_RETRIED",
		fmt.Sprintf("?deployment_id=%d", rig.plain.ID):           "NODE_REPLACED,UNIT_RETRIED",
		"?since=2999-01-01T00:00:00Z":                            "",
		"?until=2000-01-01T00:00:00Z":                            "",
		"?since=2000-01-01T00:00:00Z&until=2999-01-01T00:00:00Z": "DEPLOYMENT_UNSCHEDULABLE,NODE_REPLACED,UNIT_RETRIED,AUTO_WAKE",
		"?limit=2": "DEPLOYMENT_UNSCHEDULABLE,NODE_REPLACED",
	} {
		status, items, body := rig.history(t, rig.session, query)
		if status != http.StatusOK || historyCodesOf(items) != want {
			t.Errorf("%s = %d %s, want %q", query, status, body, want)
		}
	}
	// A cursor continues after the last item of the page before.
	_, first, _ := rig.history(t, rig.session, "?limit=3")
	cursor := fmt.Sprint(int(first[2]["id"].(float64)))
	if _, rest, _ := rig.history(t, rig.session, "?limit=3&before="+cursor); historyCodesOf(rest) != "AUTO_WAKE" {
		t.Fatalf("after cursor %s = %s", cursor, historyCodesOf(rest))
	}
	// An empty page is an empty list, not null.
	if status, _, body := rig.history(t, rig.session, "?until=2000-01-01T00:00:00Z"); status != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
		t.Fatalf("empty = %d %s", status, body)
	}
}

func TestSchedulingHistoryRefusals(t *testing.T) {
	rig := newScheduleRig(t)
	for query, want := range map[string]struct{ code, field string }{
		"?category=sometimes":                           {"FIELD_INVALID", "category"},
		"?deployment_id=abc":                            {"FIELD_INVALID", "deployment_id"},
		"?deployment_id=0":                              {"FIELD_INVALID", "deployment_id"},
		"?since=yesterday":                              {"FIELD_INVALID", "since"},
		"?until=" + url.QueryEscape("2026-10-01 12:00"): {"FIELD_INVALID", "until"},
		"?before=0":                                     {"EVENT_CURSOR_INVALID", ""},
		"?before=abc":                                   {"EVENT_CURSOR_INVALID", ""},
		"?limit=0":                                      {"EVENT_LIMIT_INVALID", ""},
		"?limit=-1":                                     {"EVENT_LIMIT_INVALID", ""},
	} {
		status, _, body := rig.history(t, rig.session, query)
		code, params := errorBody(t, body)
		if status != http.StatusBadRequest || code != want.code || (want.field != "" && params["field"] != want.field) {
			t.Errorf("%s = %d %s, want 400 %s %s", query, status, body, want.code, want.field)
		}
	}
	if status, _, _ := rig.history(t, "", ""); status != http.StatusUnauthorized {
		t.Fatalf("no credentials = %d, want 401", status)
	}
	if status, _, _ := rig.history(t, rig.mint("read"), ""); status != http.StatusOK {
		t.Fatalf("a read token = %d, want 200", status)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/scheduling/history", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

// The history outlives the deployment: a deleted one still reads by name.
func TestSchedulingHistoryOfADeletedDeployment(t *testing.T) {
	rig := newScheduleRig(t)
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindLifecycle, store.EventReplicasChanged, map[string]any{"from": 1, "to": 2})
	if err := rig.st.DeleteDeployment(rig.plain.ID); err != nil {
		t.Fatal(err)
	}
	_, items, body := rig.history(t, rig.session, "")
	if len(items) != 1 || items[0]["deployment"] != "plain" || items[0]["deployment_deleted"] != true || items[0]["deployment_id"] != float64(rig.plain.ID) {
		t.Fatalf("history = %s", body)
	}
}

// An entry that gave GPUs to another deployment names it (target), as it
// names its own: live by name, gone with deleted set, unknown by id alone.
func TestSchedulingHistoryNamesTheDeploymentAYieldWasFor(t *testing.T) {
	rig := newScheduleRig(t)
	rig.st.AddDeploymentEventCode(rig.sleepable.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "rank", "for_deployment": rig.plain.ID, "level": 1})
	rig.st.AddDeploymentEventCode(rig.sleepable.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "idle", "for_deployment": rig.multi.ID, "level": 1})
	rig.st.AddDeploymentEventCode(rig.sleepable.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "idle", "for_deployment": 9999, "level": 1})
	rig.st.AddDeploymentEventCode(rig.sleepable.ID, store.EventKindAutomation, "AUTO_WAKE", map[string]any{"trigger": "request"})
	if err := rig.st.DeleteDeployment(rig.multi.ID); err != nil {
		t.Fatal(err)
	}
	status, items, body := rig.history(t, rig.session, "")
	if status != http.StatusOK || len(items) != 4 {
		t.Fatalf("history = %d %s", status, body)
	}
	target := func(item map[string]any) string {
		raw, _ := json.Marshal(item["target"])
		return string(raw)
	}
	if got := target(items[3]); got != fmt.Sprintf(`{"id":%d,"name":"plain"}`, rig.plain.ID) {
		t.Errorf("yield for a live deployment: target = %s", got)
	}
	if got := target(items[2]); got != fmt.Sprintf(`{"deleted":true,"id":%d,"name":"multi"}`, rig.multi.ID) {
		t.Errorf("yield for a deleted deployment: target = %s", got)
	}
	if got := target(items[1]); got != `{"id":9999}` {
		t.Errorf("yield for an unknown deployment: target = %s", got)
	}
	if _, has := items[0]["target"]; has {
		t.Errorf("an entry for no one carries a target: %v", items[0])
	}
}

// Every order an operator gives through the API leaves an entry with the
// user's name, and a change that changes nothing leaves none.
func TestOperatorOrdersAreInTheHistoryWithTheirActor(t *testing.T) {
	rig := newScheduleRig(t)
	dep := rig.sleepable
	if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "replicas"), `{"replicas":2}`); rec.Code != http.StatusOK {
		t.Fatalf("scale = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "replicas"), `{"replicas":2}`); rec.Code != http.StatusOK {
		t.Fatalf("scale again = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, schedPath(dep, "sleep?level=2"), ""); rec.Code != http.StatusOK {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, schedPath(dep, "restart"), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body)
	}
	_, items, body := rig.history(t, rig.session, "")
	if historyCodesOf(items) != "DEPLOYMENT_ROLLING_RESTARTED,SLEEP_ORDERED,REPLICAS_CHANGED" {
		t.Fatalf("history = %s", body)
	}
	for _, item := range items {
		if item["actor"] != "admin" || item["category"] != "manual" {
			t.Errorf("%v: want the logged-in user as actor in the manual category", item)
		}
	}
	params := items[1]["params"].(map[string]any)
	if params["level"] != float64(2) || params["mode"] != "wait" || params["scope"] != "deployment" {
		t.Errorf("sleep params = %v", params)
	}
	if params = items[2]["params"].(map[string]any); params["from"] != float64(1) || params["to"] != float64(2) {
		t.Errorf("scale params = %v", params)
	}
}

// A replica's sleep and wake each leave an entry listing the instances; a
// request that carries no user (the handlers called directly) names no actor.
func TestInstanceSleepAndWakeOrdersAreInTheTimeline(t *testing.T) {
	f, dep := replicaSleepFixture(t)
	if rec := instanceRequest(f.h.sleepInstance, "plain-0", "?level=2"); rec.Code != http.StatusOK {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body)
	}
	reportAsleep(t, f.st, "plain-0", 2)
	if rec := instanceRequest(f.h.wakeInstance, "plain-0", ""); rec.Code != http.StatusOK {
		t.Fatalf("wake = %d %s", rec.Code, rec.Body)
	}
	events, err := f.st.SchedulingHistory(store.SchedulingHistoryFilter{DeploymentID: dep.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Code != store.EventWakeOrdered || events[1].Code != store.EventSleepOrdered {
		t.Fatalf("events = %+v", events)
	}
	if got := events[0].Data; got != `{"instances":["plain-0"],"node_uuids":["sleep-node"]}` {
		t.Errorf("wake params = %s", got)
	}
	if got := events[1].Data; got != `{"instances":["plain-0"],"level":2,"mode":"wait","node_uuids":["sleep-node"],"scope":"replica"}` {
		t.Errorf("sleep params = %s", got)
	}
}

// Each entry names the nodes it concerns: those its params name (by UUID,
// or by name on older rows), else those the live instances it names are on;
// a gone node keeps a name and has no UUID to open; an entry with neither
// carries no nodes.
func TestSchedulingHistoryNamesTheNodesOfEachEntry(t *testing.T) {
	rig := newScheduleRig(t)
	gpuA := store.Node{UUID: "node-hist-a", Name: "gpu-a"}
	gpuB := store.Node{UUID: "node-hist-b", Hostname: "host-b"}
	for _, n := range []*store.Node{&gpuA, &gpuB} {
		if err := rig.st.DB().Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.st.DB().Create(&store.Instance{UUID: "hist-inst-a", DeploymentID: rig.plain.ID, NodeID: gpuA.ID}).Error; err != nil {
		t.Fatal(err)
	}
	// Rows as they were written before events kept their nodes.
	legacy := func(code, kind, data string) {
		if err := rig.st.DB().Create(&store.InstanceEvent{DeploymentID: rig.plain.ID, Kind: kind, Code: code, Data: data, CreatedAt: time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	legacy("AUTO_WAKE", store.EventKindAutomation, `{"trigger":"request","instances":["hist-inst-a"]}`)
	legacy("AUTO_WAKE", store.EventKindAutomation, `{"trigger":"request","instances":["hist-inst-gone"]}`)
	legacy(store.EventReplicaRebuilt, store.EventKindLifecycle, `{"replica":1,"node":"gpu-old"}`)
	legacy("NODE_REPLACE_NO_CANDIDATE", store.EventKindAutomation, `{"nodes":["gpu-x","gpu-y"]}`)
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"node": gpuB.UUID})
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindAutomation, "NODE_REPLACED", map[string]any{store.EventParamNodeUUIDs: []string{"0123456789abcdef", gpuA.UUID}})
	// Written now, an event naming an instance keeps the instance's node.
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindLifecycle, store.EventWakeOrdered, map[string]any{"instances": []string{"hist-inst-a"}})

	status, items, body := rig.history(t, rig.session, "")
	if status != http.StatusOK || len(items) != 7 {
		t.Fatalf("history = %d %s", status, body)
	}
	nodes := func(item map[string]any) string {
		if _, has := item["nodes"]; !has {
			return "absent"
		}
		raw, _ := json.Marshal(item["nodes"])
		return string(raw)
	}
	for i, want := range []string{
		`[{"name":"gpu-a","uuid":"node-hist-a"}]`,                     // WAKE_ORDERED: kept at write time
		`[{"name":"01234567"},{"name":"gpu-a","uuid":"node-hist-a"}]`, // NODE_REPLACED: a gone node by its short UUID
		`[{"name":"host-b","uuid":"node-hist-b"}]`,                    // DEPLOYMENT_UNSCHEDULABLE: "node" is a UUID
		`[{"name":"gpu-x"},{"name":"gpu-y"}]`,                         // older NODE_REPLACE_NO_CANDIDATE: names only
		`[{"name":"gpu-old"}]`,                                        // older REPLICA_REBUILT: a name only
		`absent`,                                                      // the instance is gone
		`[{"name":"gpu-a","uuid":"node-hist-a"}]`,                     // older AUTO_WAKE: the live instance's node
	} {
		if got := nodes(items[i]); got != want {
			t.Errorf("%s (item %d) nodes = %s, want %s", items[i]["code"], i, got, want)
		}
	}
	if params := items[0]["params"].(map[string]any); fmt.Sprint(params[store.EventParamNodeUUIDs]) != "[node-hist-a]" {
		t.Errorf("WAKE_ORDERED params = %v, want the node kept", params)
	}
}

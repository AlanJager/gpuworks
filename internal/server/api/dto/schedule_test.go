package dto

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestLifecycleScheduleWire(t *testing.T) {
	next := time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   LifecycleSchedule
		wire string
	}{
		{
			name: "sleep order",
			in:   LifecycleSchedule{ID: 7, Action: "sleep", Weekdays: []int{1, 2, 3, 4, 5}, Time: "21:00", Timezone: "Asia/Shanghai", SleepLevel: 2, Enabled: true, NextAt: &next},
			wire: `{"id":7,"action":"sleep","weekdays":[1,2,3,4,5],"time":"21:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T13:00:00Z"}`,
		},
		{
			name: "wake order omits level, replicas and a disabled order's next_at",
			in:   LifecycleSchedule{ID: 8, Action: "wake", Weekdays: []int{0, 6}, Time: "08:30", Timezone: "", Enabled: false},
			wire: `{"id":8,"action":"wake","weekdays":[0,6],"time":"08:30","timezone":"","enabled":false}`,
		},
		{
			name: "replicas order",
			in:   LifecycleSchedule{ID: 9, Action: "replicas", Weekdays: []int{3}, Time: "00:00", Timezone: "UTC", Replicas: 4, Enabled: true, NextAt: &next},
			wire: `{"id":9,"action":"replicas","weekdays":[3],"time":"00:00","timezone":"UTC","replicas":4,"enabled":true,"next_at":"2026-10-26T13:00:00Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := marshalString(t, tc.in); got != tc.wire {
				t.Fatalf("marshal\n got %s\nwant %s", got, tc.wire)
			}
			var back LifecycleSchedule
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("round trip = %+v, want %+v", back, tc.in)
			}
		})
	}
}

func TestLifecycleScheduleRequestWire(t *testing.T) {
	var sleep LifecycleScheduleRequest
	if err := json.Unmarshal([]byte(`{"action":"sleep","weekdays":[1,5],"time":"21:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":false}`), &sleep); err != nil {
		t.Fatal(err)
	}
	if sleep.Action != "sleep" || !reflect.DeepEqual(sleep.Weekdays, []int{1, 5}) || sleep.Time != "21:00" || sleep.Timezone != "Asia/Shanghai" ||
		sleep.SleepLevel == nil || *sleep.SleepLevel != 2 || sleep.Replicas != nil || sleep.Enabled == nil || *sleep.Enabled {
		t.Fatalf("decoded = %+v", sleep)
	}

	// The optional fields are pointers so "absent" and "zero" differ: enabled
	// defaults to true only when absent, and replicas:0 is an explicit (invalid) 0.
	var minimal LifecycleScheduleRequest
	if err := json.Unmarshal([]byte(`{"action":"wake","weekdays":[0],"time":"07:00"}`), &minimal); err != nil {
		t.Fatal(err)
	}
	if minimal.SleepLevel != nil || minimal.Replicas != nil || minimal.Enabled != nil || minimal.Timezone != "" {
		t.Fatalf("absent optional fields decoded as %+v", minimal)
	}
	var zero LifecycleScheduleRequest
	if err := json.Unmarshal([]byte(`{"action":"replicas","weekdays":[0],"time":"07:00","replicas":0}`), &zero); err != nil || zero.Replicas == nil || *zero.Replicas != 0 {
		t.Fatalf("explicit replicas:0 = %+v %v", zero, err)
	}

	level, replicas, on := 1, 3, true
	if got, want := marshalString(t, LifecycleScheduleRequest{Action: "sleep", Weekdays: []int{2}, Time: "22:15", Timezone: "UTC", SleepLevel: &level, Enabled: &on}),
		`{"action":"sleep","weekdays":[2],"time":"22:15","timezone":"UTC","sleep_level":1,"enabled":true}`; got != want {
		t.Fatalf("marshal\n got %s\nwant %s", got, want)
	}
	if got, want := marshalString(t, LifecycleScheduleRequest{Action: "replicas", Weekdays: []int{2}, Time: "22:15", Replicas: &replicas}),
		`{"action":"replicas","weekdays":[2],"time":"22:15","timezone":"","replicas":3}`; got != want {
		t.Fatalf("marshal\n got %s\nwant %s", got, want)
	}

	// The request is strict on the server (DisallowUnknownFields), so the wire
	// must not grow a key the handler would not name.
	dec := json.NewDecoder(bytes.NewBufferString(`{"action":"wake","weekdays":[1],"time":"07:00","next_at":"2026-01-01T00:00:00Z"}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&LifecycleScheduleRequest{}); err == nil {
		t.Fatal("the response-only key next_at was accepted in a request")
	}
}

func TestIdleSleepAndNodeReplacementWire(t *testing.T) {
	if got, want := marshalString(t, IdleSleep{AfterSec: 900, Level: 2}), `{"after_sec":900,"level":2}`; got != want {
		t.Fatalf("idle sleep = %s, want %s", got, want)
	}
	// Off is after_sec 0, and it is still written: no omitempty.
	if got, want := marshalString(t, IdleSleep{Level: 1}), `{"after_sec":0,"level":1}`; got != want {
		t.Fatalf("idle sleep off = %s, want %s", got, want)
	}
	if got, want := marshalString(t, NodeReplacement{AfterSec: 600}), `{"after_sec":600}`; got != want {
		t.Fatalf("node replacement = %s, want %s", got, want)
	}
	if got, want := marshalString(t, NodeReplacement{}), `{"after_sec":0}`; got != want {
		t.Fatalf("node replacement never = %s, want %s", got, want)
	}
	var idle IdleSleep
	if err := json.Unmarshal([]byte(`{"after_sec":3600,"level":1}`), &idle); err != nil || idle != (IdleSleep{AfterSec: 3600, Level: 1}) {
		t.Fatalf("idle sleep decoded = %+v %v", idle, err)
	}
	var repl NodeReplacement
	if err := json.Unmarshal([]byte(`{"after_sec":120}`), &repl); err != nil || repl != (NodeReplacement{AfterSec: 120}) {
		t.Fatalf("node replacement decoded = %+v %v", repl, err)
	}
	if err := json.Unmarshal([]byte(`{"after_sec":"120"}`), &repl); err == nil {
		t.Fatal("a string after_sec was accepted")
	}
}

func TestWakeWaitResultWire(t *testing.T) {
	waiting := WakeWaitResult{Status: "waiting", Woken: []string{}, Waiting: []string{"inst-1"}, Yielded: []string{"other"}}
	const wire = `{"status":"waiting","woken":[],"waiting":["inst-1"],"yielded":["other"]}`
	if got := marshalString(t, waiting); got != wire {
		t.Fatalf("marshal\n got %s\nwant %s", got, wire)
	}
	var back WakeWaitResult
	if err := json.Unmarshal([]byte(wire), &back); err != nil || !reflect.DeepEqual(back, waiting) {
		t.Fatalf("round trip = %+v %v", back, err)
	}
	// The lists are never omitted: an empty one is [] so a client can iterate
	// without a nil check. (A nil slice would encode as null; the handler
	// builds empty ones.)
	if got, want := marshalString(t, WakeWaitResult{Status: "waking", Woken: []string{"a"}, Waiting: []string{}, Yielded: []string{}}),
		`{"status":"waking","woken":["a"],"waiting":[],"yielded":[]}`; got != want {
		t.Fatalf("waking = %s, want %s", got, want)
	}
}

// The deployment carries the automation settings on every response:
// idle_sleep and node_replacement always, wake_waiting_since only while a wake
// waits.
func TestDeploymentSchedulingFieldsWire(t *testing.T) {
	keys := func(d Deployment) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(marshalString(t, d)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := keys(Deployment{IdleSleep: IdleSleep{AfterSec: 900, Level: 2}, NodeReplacement: NodeReplacement{AfterSec: 600}})
	if string(m["idle_sleep"]) != `{"after_sec":900,"level":2}` || string(m["node_replacement"]) != `{"after_sec":600}` {
		t.Fatalf("idle_sleep=%s node_replacement=%s", m["idle_sleep"], m["node_replacement"])
	}
	if _, ok := m["wake_waiting_since"]; ok {
		t.Fatalf("wake_waiting_since present with no waiting wake: %s", m["wake_waiting_since"])
	}
	since := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	m = keys(Deployment{WakeWaitingSince: &since})
	if string(m["wake_waiting_since"]) != `"2026-10-25T01:02:03Z"` {
		t.Fatalf("wake_waiting_since = %s", m["wake_waiting_since"])
	}
	if string(m["idle_sleep"]) != `{"after_sec":0,"level":0}` || string(m["node_replacement"]) != `{"after_sec":0}` {
		t.Fatalf("zero settings must still be written: %s %s", m["idle_sleep"], m["node_replacement"])
	}
	var back Deployment
	if err := json.Unmarshal([]byte(`{"idle_sleep":{"after_sec":60,"level":1},"node_replacement":{"after_sec":7200},"wake_waiting_since":"2026-10-25T01:02:03Z"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.IdleSleep != (IdleSleep{AfterSec: 60, Level: 1}) || back.NodeReplacement.AfterSec != 7200 || back.WakeWaitingSince == nil || !back.WakeWaitingSince.Equal(since) {
		t.Fatalf("decoded = %+v", back)
	}
}

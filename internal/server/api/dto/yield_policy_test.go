package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// The wire form of a policy: a parameter that does not apply is absent
// rather than zero.
func TestYieldPolicyWire(t *testing.T) {
	defaults := func(yield string) servicelevel.Policy {
		p, _ := servicelevel.Parse(yield, servicelevel.Overrides{})
		return p
	}
	for _, c := range []struct {
		name string
		p    servicelevel.Policy
		wire string
	}{
		{"never", defaults("never"), `{"policy":"never"}`},
		{"idle", defaults("idle"), `{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}`},
		{"timeslice", defaults("timeslice"), `{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		{"anytime", defaults("anytime"), `{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}`},
		{"anytime waiting", servicelevel.Policy{Yield: "anytime", IdleSec: 15, SleepLevel: "l2", Unfinished: "wait", UnfinishedWaitSec: 45, HoldMaxSec: 10},
			`{"policy":"anytime","idle_sec":15,"sleep_level":"l2","unfinished":"wait","unfinished_wait_sec":45,"hold_max_sec":10}`},
		{"leftover parameters are dropped", servicelevel.Policy{Yield: "never", IdleSec: 99, SleepLevel: "l2", MaxServeSec: 5, HoldMaxSec: 50},
			`{"policy":"never"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(YieldPolicyOf(c.p))
			if err != nil || string(got) != c.wire {
				t.Fatalf("wire\n got %s\nwant %s (err %v)", got, c.wire, err)
			}
			var back YieldPolicy
			if err := json.Unmarshal([]byte(c.wire), &back); err != nil || !reflect.DeepEqual(back, YieldPolicyOf(c.p)) {
				t.Fatalf("round trip = %+v %v", back, err)
			}
		})
	}
	withNeighbours := YieldPolicyOf(defaults("idle"))
	withNeighbours.IdleNeighbours = []string{"chat-b"}
	if got := marshalString(t, withNeighbours); got != `{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300,"idle_neighbours":["chat-b"]}` {
		t.Fatalf("with idle neighbours = %s", got)
	}
}

func TestYieldPolicyRequestParse(t *testing.T) {
	parse := func(body string) (servicelevel.Policy, *servicelevel.Violation) {
		t.Helper()
		var req YieldPolicyRequest
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return req.Parse()
	}
	// The full form, as the wire of a response sent back (its neighbours
	// ignored).
	p, v := parse(`{"policy":"idle","idle_sec":30,"sleep_level":"l1","hold_max_sec":120,"idle_neighbours":["x"]}`)
	if v != nil || p != (servicelevel.Policy{Yield: "idle", IdleSec: 30, SleepLevel: "l1", HoldMaxSec: 120}) {
		t.Errorf("full form = %+v %+v", p, v)
	}
	// A policy alone takes the policy's defaults.
	want, _ := servicelevel.Parse(servicelevel.TimeSlice, servicelevel.Overrides{})
	if p, v = parse(`{"policy":"timeslice"}`); v != nil || p != want {
		t.Errorf("policy alone = %+v %+v", p, v)
	}
	if p, v = parse(`{"policy":"timeslice","max_serve_sec":60,"idle_sec":15}`); v != nil || p.MaxServeSec != 60 || p.IdleSec != 15 {
		t.Errorf("timeslice turn = %+v %+v", p, v)
	}
	for _, c := range []struct {
		body string
		want servicelevel.Violation
	}{
		{`{}`, servicelevel.Violation{Field: "policy", Reason: "required"}},
		{`{"idle_sec":60}`, servicelevel.Violation{Field: "policy", Reason: "required"}},
		{`{"policy":"gold"}`, servicelevel.Violation{Field: "policy", Reason: "invalid_value"}},
		{`{"policy":"idle","idle_sec":14}`, servicelevel.Violation{Field: "idle_sec", Reason: "out_of_range", Min: 15, Max: 604800}},
		{`{"policy":"timeslice","max_serve_sec":29}`, servicelevel.Violation{Field: "max_serve_sec", Reason: "out_of_range", Min: 30, Max: 86400}},
	} {
		if _, v := parse(c.body); v == nil || *v != c.want {
			t.Errorf("%s = %+v, want %+v", c.body, v, c.want)
		}
	}
	for _, gone := range []string{`{"preset":"online"}`, `{"policy":"never","auto_sleep":true}`, `{"policy":"timeslice","min_serve_sec":60}`} {
		var req YieldPolicyRequest
		dec := json.NewDecoder(strings.NewReader(gone))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err == nil {
			t.Errorf("%s was taken: the field is gone", gone)
		}
	}
}

// A deployment carries yield_policy on every response, and the create request
// takes it.
func TestDeploymentYieldPolicyWire(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(marshalString(t, Deployment{YieldPolicy: YieldPolicyOf(servicelevel.Policy{Yield: "never"})})), &m); err != nil {
		t.Fatal(err)
	}
	if string(m["yield_policy"]) != `{"policy":"never"}` {
		t.Fatalf("yield_policy = %s", m["yield_policy"])
	}
	for _, gone := range []string{"service_level", "idle_sleep"} {
		if _, ok := m[gone]; ok {
			t.Fatalf("%s is still on the deployment: %s", gone, m[gone])
		}
	}
	var req DeployRequest
	if err := json.Unmarshal([]byte(`{"yield_policy":{"policy":"anytime"}}`), &req); err != nil || req.YieldPolicy == nil || req.YieldPolicy.Policy != "anytime" {
		t.Fatalf("create request = %+v %v", req.YieldPolicy, err)
	}
	if got := marshalString(t, DeployRequest{}); strings.Contains(got, "yield_policy") {
		t.Fatalf("an absent yield_policy is written: %s", got)
	}
}

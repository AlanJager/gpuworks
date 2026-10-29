package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// The wire form of a policy: the preset is derived, and a parameter that does
// not apply is absent rather than zero.
func TestYieldPolicyWire(t *testing.T) {
	preset := func(name string) servicelevel.Policy { p, _ := servicelevel.PresetPolicy(name); return p }
	for _, c := range []struct {
		name string
		p    servicelevel.Policy
		wire string
	}{
		{"resident", preset("resident"), `{"preset":"resident","policy":"never","auto_sleep":false}`},
		{"online", preset("online"), `{"preset":"online","policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}`},
		{"shared", preset("shared"), `{"preset":"shared","policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		{"offline", preset("offline"), `{"preset":"offline","policy":"anytime","auto_sleep":false,"sleep_level":"auto","unfinished":"abort","hold_max_sec":1800}`},
		{"never with auto sleep", servicelevel.Policy{Yield: "never", AutoSleep: true, IdleSec: 600, SleepLevel: "l1", HoldMaxSec: 120},
			`{"preset":"custom","policy":"never","auto_sleep":true,"idle_sec":600,"sleep_level":"l1","hold_max_sec":120}`},
		{"timeslice with a zero turn and an unbounded wait", servicelevel.Policy{Yield: "timeslice", IdleSec: 30, SleepLevel: "l2", MinServeSec: 0, Unfinished: "wait_unbounded", HoldMaxSec: 10},
			`{"preset":"custom","policy":"timeslice","auto_sleep":false,"idle_sec":30,"sleep_level":"l2","min_serve_sec":0,"unfinished":"wait_unbounded","hold_max_sec":10}`},
		{"leftover parameters are dropped", servicelevel.Policy{Yield: "never", IdleSec: 99, SleepLevel: "l2", MinServeSec: 5, HoldMaxSec: 50},
			`{"preset":"resident","policy":"never","auto_sleep":false}`},
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
	for preset, policy := range map[string]string{"resident": "never", "online": "idle", "shared": "timeslice", "offline": "anytime"} {
		p, v := parse(`{"preset":"` + preset + `"}`)
		if v != nil || p.Yield != policy || servicelevel.PresetOf(p) != preset {
			t.Errorf("preset %s = %+v %+v", preset, p, v)
		}
	}
	// The full form, as the wire of a response sent back.
	p, v := parse(`{"preset":"custom","policy":"idle","auto_sleep":true,"idle_sec":30,"sleep_level":"l1","hold_max_sec":120}`)
	if v != nil || p != (servicelevel.Policy{Yield: "idle", AutoSleep: true, IdleSec: 30, SleepLevel: "l1", HoldMaxSec: 120}) {
		t.Errorf("full form = %+v %+v", p, v)
	}
	// A policy alone takes the policy's defaults.
	if p, v = parse(`{"policy":"timeslice"}`); v != nil || servicelevel.PresetOf(p) != "shared" {
		t.Errorf("policy alone = %+v %+v", p, v)
	}
	for _, c := range []struct {
		body string
		want servicelevel.Violation
	}{
		{`{}`, servicelevel.Violation{Field: "policy", Reason: "required"}},
		{`{"preset":"online","idle_sec":60}`, servicelevel.Violation{Field: "policy", Reason: "required"}},
		{`{"idle_sec":60}`, servicelevel.Violation{Field: "policy", Reason: "required"}},
		{`{"preset":"custom"}`, servicelevel.Violation{Field: "preset", Reason: "invalid_value"}},
		{`{"preset":"gold"}`, servicelevel.Violation{Field: "preset", Reason: "invalid_value"}},
		{`{"policy":"gold"}`, servicelevel.Violation{Field: "policy", Reason: "invalid_value"}},
		{`{"policy":"idle","idle_sec":29}`, servicelevel.Violation{Field: "idle_sec", Reason: "out_of_range", Min: 30, Max: 604800}},
	} {
		if _, v := parse(c.body); v == nil || *v != c.want {
			t.Errorf("%s = %+v, want %+v", c.body, v, c.want)
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
	if string(m["yield_policy"]) != `{"preset":"resident","policy":"never","auto_sleep":false}` {
		t.Fatalf("yield_policy = %s", m["yield_policy"])
	}
	for _, gone := range []string{"service_level", "idle_sleep"} {
		if _, ok := m[gone]; ok {
			t.Fatalf("%s is still on the deployment: %s", gone, m[gone])
		}
	}
	var req DeployRequest
	if err := json.Unmarshal([]byte(`{"yield_policy":{"preset":"offline"}}`), &req); err != nil || req.YieldPolicy == nil || req.YieldPolicy.Preset != "offline" {
		t.Fatalf("create request = %+v %v", req.YieldPolicy, err)
	}
	if got := marshalString(t, DeployRequest{}); strings.Contains(got, "yield_policy") {
		t.Fatalf("an absent yield_policy is written: %s", got)
	}
}

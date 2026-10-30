package dto

import (
	"encoding/json"
	"testing"
)

// The experience status carries the password only while the environment
// runs, and a stop reason only when there is one.
func TestExperienceStatusWire(t *testing.T) {
	for _, tc := range []struct {
		in   ExperienceStatus
		want string
	}{
		{ExperienceStatus{Port: 8810, User: "admin"}, `{"enabled":false,"ready":false,"port":8810,"user":"admin"}`},
		{ExperienceStatus{Port: 8810, User: "admin", Error: "exit status 1"}, `{"enabled":false,"ready":false,"port":8810,"user":"admin","error":"exit status 1"}`},
		{ExperienceStatus{Enabled: true, Ready: true, Port: 8810, User: "admin", Password: "5d706bcd0e1b2eaa"},
			`{"enabled":true,"ready":true,"port":8810,"user":"admin","password":"5d706bcd0e1b2eaa"}`},
	} {
		raw, err := json.Marshal(tc.in)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("marshal = %s, %v; want %s", raw, err, tc.want)
		}
		var back ExperienceStatus
		if err := json.Unmarshal(raw, &back); err != nil || back != tc.in {
			t.Fatalf("round trip = %+v, %v", back, err)
		}
	}
}

// enabled must be given: absent and null decode to nil, which the handler
// refuses, rather than to false.
func TestExperienceEnabledWire(t *testing.T) {
	for body, want := range map[string]*bool{`{}`: nil, `{"enabled":null}`: nil} {
		var got ExperienceEnabled
		if err := json.Unmarshal([]byte(body), &got); err != nil || got.Enabled != want {
			t.Fatalf("%s = %v, %v", body, got.Enabled, err)
		}
	}
	var got ExperienceEnabled
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &got); err != nil || got.Enabled == nil || *got.Enabled {
		t.Fatalf("false = %v, %v", got.Enabled, err)
	}
}

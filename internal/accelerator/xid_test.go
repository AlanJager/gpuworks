package accelerator

import (
	"strconv"
	"testing"
)

// Pin every named Xid independently of the implementation: changing a class
// can either hide a broken card or keep a healthy one out of service.
func TestXidFinding(t *testing.T) {
	cases := []struct {
		xid  int
		want string
	}{
		{79, HealthRebootRequired}, // GPU has fallen off the bus

		{61, HealthResetRequired},  // internal micro-controller breakpoint
		{62, HealthResetRequired},  // internal micro-controller halt
		{95, HealthResetRequired},  // uncontained memory error
		{119, HealthResetRequired}, // GSP RPC timeout
		{120, HealthResetRequired}, // GSP error
		{140, HealthResetRequired}, // unrecovered ECC error

		// Application errors end the process, not the card.
		{13, ""}, {31, ""}, {43, ""}, {45, ""}, {68, ""}, {69, ""}, {94, ""},

		// These are judged from the driver's current state.
		{48, ""}, {63, ""}, {64, ""}, {154, ""},

		// NVLink events are judged from the links themselves.
		{74, ""}, {144, ""}, {145, ""}, {146, ""}, {147, ""}, {148, ""}, {149, ""}, {150, ""},

		// Unknown events are recorded without guessing a finding.
		{0, ""}, {1, ""}, {2, ""}, {143, ""}, {151, ""}, {200, ""}, {-1, ""},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.xid), func(t *testing.T) {
			if got := XidFinding(tc.xid); got != tc.want {
				t.Errorf("XidFinding(%d) = %q, want %q", tc.xid, got, tc.want)
			}
		})
	}
}

func TestXidNVLink(t *testing.T) {
	cases := []struct {
		xid  int
		want bool
	}{
		{74, true},
		{144, true}, {145, true}, {146, true}, {147, true},
		{148, true}, {149, true}, {150, true},
		{73, false}, {75, false}, {143, false}, {151, false},
		{13, false}, {79, false}, {140, false},
		{0, false}, {-1, false}, {200, false},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.xid), func(t *testing.T) {
			if got := XidNVLink(tc.xid); got != tc.want {
				t.Errorf("XidNVLink(%d) = %v, want %v", tc.xid, got, tc.want)
			}
		})
	}
}

// An event judged from link state must never also latch a card finding.
func TestXidClassesDoNotOverlap(t *testing.T) {
	for xid := 0; xid <= 200; xid++ {
		if finding := XidFinding(xid); XidNVLink(xid) && finding != "" {
			t.Errorf("Xid %d is both an NVLink event and a finding (%q)", xid, finding)
		}
	}
}

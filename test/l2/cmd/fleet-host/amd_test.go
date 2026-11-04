package main

import "testing"

func TestFleetAMDPercentRequiresExplicitBoundedConfiguration(t *testing.T) {
	for _, raw := range []string{"-1", "101", "1.5", "foo"} {
		if _, err := fleetAMDPercent(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for raw, want := range map[string]int{"": 0, "0": 0, "50": 50, "100": 100} {
		got, err := fleetAMDPercent(raw)
		if err != nil || got != want {
			t.Fatalf("%q=%d,%v", raw, got, err)
		}
	}
	if fleetAMDNode("stable-node", 0) || !fleetAMDNode("stable-node", 100) {
		t.Fatal("percent endpoints changed")
	}
}

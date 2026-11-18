package main

import (
	"strconv"
	"testing"
)

// Each periodic push holds every node and differs from the push before in
// 1% of the entries (at least one), a different slice each round, and the
// slices wrap around.
func TestPeriodicHoldsChangeOnePercentPerRound(t *testing.T) {
	codes := func(size, round int) map[string]string {
		table := holdsTable(size, round)
		if len(table.Nodes) != size {
			t.Fatalf("size %d round %d: %d entries", size, round, len(table.Nodes))
		}
		out := map[string]string{}
		for id, n := range table.Nodes {
			if n.Code != "ACME_QUARANTINE" && n.Code != "ACME_MAINTENANCE" {
				t.Fatalf("entry %s has code %q", id, n.Code)
			}
			out[id] = n.Code
		}
		return out
	}
	diff := func(size, round int) []string {
		a, b := codes(size, round-1), codes(size, round)
		var out []string
		for i := 1; i <= size; i++ {
			if id := strconv.Itoa(i); a[id] != b[id] {
				out = append(out, id)
			}
		}
		return out
	}
	for _, c := range []struct {
		size, round int
		changed     int
		first       string
	}{
		{size: 1000, round: 1, changed: 10, first: "11"},
		{size: 1000, round: 99, changed: 10, first: "991"},
		{size: 1000, round: 100, changed: 10, first: "1"}, // wrapped: slice 0 flips back
		{size: 1000, round: 250, changed: 10, first: "501"},
		{size: 50, round: 3, changed: 1, first: "4"},
		{size: 150, round: 2, changed: 1, first: "3"},
		{size: 1, round: 7, changed: 1, first: "1"},
	} {
		got := diff(c.size, c.round)
		if len(got) != c.changed || got[0] != c.first {
			t.Errorf("size %d round %d: changed %v, want %d entries from %s", c.size, c.round, got, c.changed, c.first)
		}
	}
	if first := codes(1000, 0); first["1"] != "ACME_QUARANTINE" || first["11"] != "ACME_MAINTENANCE" {
		t.Fatal("round 0 does not flip only the first slice")
	}
	// A full cycle and a second one return every entry to where it began.
	if a, b := codes(1000, 0), codes(1000, 200); len(a) != len(b) || a["1"] != b["1"] || a["995"] != b["995"] {
		t.Fatal("two full cycles do not return to the start")
	}
}

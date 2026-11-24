package main

import "testing"

// The fixture holds every way of hiding state the line-matching ratchet let
// through: several names on one line, a lock by pointer, sync.Map, a map type
// of the package, fields wrapped in an anonymous struct, RPC cases with
// several values or moved to a helper switch, compared with == or keyed in a
// table of handlers on the type or of closures over it, methods on a value
// receiver, a function or another type's method that takes the type. Methods
// in test files, a constructor, and a named subsystem's own fields, handler
// table and methods that do not take the type do not count.
func TestCountSeesHiddenState(t *testing.T) {
	for _, tc := range []struct {
		measure string
		want    int
	}{
		// aMu bMu cMu m1 m2 cache seen nested(+dMu byID) images Mutex
		{"fields", 12},
		// aMu bMu cMu nested.dMu Mutex(embedded)
		{"mutexes", 5},
		// m1 m2 cache seen nested.byID
		{"maps", 5},
		// OnRPC more testOnlyTarget helper route table
		{"methods", 6},
		// Ping Doctor Smoke Restart Upgrade Route Closure
		{"rpcs", 7},
	} {
		got, err := count("testdata/root", "Root", tc.measure)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s = %d, want %d", tc.measure, got, tc.want)
		}
	}
}

func TestCountRefusesWhatItCannotMeasure(t *testing.T) {
	for _, tc := range []struct{ dir, typ, measure string }{
		{"testdata/root", "Missing", "fields"},
		{"testdata/root", "uuidSet", "fields"},
		{"testdata/root", "Root", "lines"},
		{"testdata/none", "Root", "fields"},
	} {
		if _, err := count(tc.dir, tc.typ, tc.measure); err == nil {
			t.Errorf("count(%q, %q, %q) succeeded", tc.dir, tc.typ, tc.measure)
		}
	}
}

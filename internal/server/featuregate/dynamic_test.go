package featuregate

import (
	"reflect"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func keysOf(states []State) []Key {
	out := make([]Key, len(states))
	for i, s := range states {
		out[i] = s.Key
	}
	return out
}

// A dynamic declaration is answered at once by the Source it was declared
// with, withdrawn by the next Replace, and never takes a static key.
func TestDynamicDeclarationsJoinAndLeaveTheGate(t *testing.T) {
	static := []Feature{{Key: "audit"}}
	d := NewDynamicSet(static)
	g, err := NewWithDynamic(static, newSource(map[Key]Availability{"audit": {Access: Full}}), d)
	if err != nil {
		t.Fatal(err)
	}
	if g.Declared("acme.backup") {
		t.Fatal("a key no one declared is declared")
	}
	if a, err := g.Require("acme.backup", false); a != Hidden || err == nil || err.Code() != messages.ErrorFeatureUnavailable {
		t.Fatalf("undeclared = %v, %v", a, err)
	}
	plugins := newSource(map[Key]Availability{"acme.backup": {Access: ReadOnly}})
	if err := d.Replace([]Feature{{Key: "acme.backup"}, {Key: "acme.vault", Presentation: Conceal}}, plugins); err != nil {
		t.Fatal(err)
	}
	if !g.Declared("acme.backup") || g.Access("acme.backup") != ReadOnly {
		t.Fatalf("acme.backup after Replace = %v", g.Access("acme.backup"))
	}
	if a, err := g.Require("acme.backup", true); a != ReadOnly || err == nil || err.Code() != messages.ErrorFeatureReadOnly {
		t.Fatalf("a write to a read-only dynamic feature = %v, %v", a, err)
	}
	// The plugins' Source does not answer for acme.vault: locked, and
	// concealed, so hidden.
	if g.Access("acme.vault") != Hidden {
		t.Fatalf("acme.vault = %v", g.Access("acme.vault"))
	}
	if g.Access("audit") != Full {
		t.Fatal("a static key is answered by the dynamic Source")
	}
	if err := d.Replace(nil, plugins); err != nil {
		t.Fatal(err)
	}
	if g.Declared("acme.backup") || g.Access("acme.backup") != Hidden {
		t.Fatal("a withdrawn key is still declared")
	}
}

func TestReplaceRefusesAndChangesNothing(t *testing.T) {
	d := NewDynamicSet([]Feature{{Key: "audit"}})
	g, err := NewWithDynamic([]Feature{{Key: "audit"}}, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Replace([]Feature{{Key: "acme.backup"}}, nil); err != nil {
		t.Fatal(err)
	}
	for name, fs := range map[string][]Feature{
		"static key":   {{Key: "acme.tools"}, {Key: "audit"}},
		"invalid key":  {{Key: "Bad"}},
		"duplicate":    {{Key: "acme.tools"}, {Key: "acme.tools"}},
		"presentation": {{Key: "acme.tools", Presentation: 9}},
	} {
		if err := d.Replace(fs, nil); err == nil {
			t.Errorf("%s: Replace accepted %v", name, fs)
		}
	}
	if got := keysOf(g.States()); !reflect.DeepEqual(got, []Key{"acme.backup", "audit"}) {
		t.Fatalf("after refused Replaces the gate declares %v", got)
	}
	// Without a Source a dynamic key fails closed like a static one.
	if a, err := g.Require("acme.backup", false); a != Locked || err == nil || err.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("dynamic key without a source = %v, %v", a, err)
	}
}

// A Gate must be built over the static features its set was made beside.
func TestNewWithDynamicRefusesAKeyTheSetDoesNotReserve(t *testing.T) {
	d := NewDynamicSet(nil)
	if _, err := NewWithDynamic([]Feature{{Key: "audit"}}, nil, d); err == nil {
		t.Fatal("a static key declared after the set was made was accepted")
	}
	if g, err := NewWithDynamic([]Feature{{Key: "audit"}}, nil, nil); err != nil || !g.Declared("audit") {
		t.Fatalf("a nil set = %v, %v", g, err)
	}
}

// An empty set leaves States exactly as the static Gate has it; a
// non-empty one is merged in key order.
func TestStatesMergeStaticAndDynamicSorted(t *testing.T) {
	static := []Feature{{Key: "zeta"}, {Key: "audit", Presentation: Conceal}, {Key: "logs"}}
	src := newSource(map[Key]Availability{"audit": {Access: Full}, "logs": {Access: Locked, Link: "https://example.com"}})
	plain, _ := New(static, src)
	d := NewDynamicSet(static)
	g, err := NewWithDynamic(static, src, d)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.States(), plain.States()) {
		t.Fatalf("an empty dynamic set changed States:\n%+v\nvs\n%+v", g.States(), plain.States())
	}
	if err := d.Replace([]Feature{{Key: "acme.tools"}, {Key: "mid.key"}, {Key: "zz.last"}}, newSource(map[Key]Availability{"mid.key": {Access: Full}})); err != nil {
		t.Fatal(err)
	}
	got := g.States()
	if keys := keysOf(got); !reflect.DeepEqual(keys, []Key{"acme.tools", "audit", "logs", "mid.key", "zeta", "zz.last"}) {
		t.Fatalf("merged keys = %v", keys)
	}
	if got[3].Access != Full || got[0].Access != Hidden || got[1].Access != Full || got[2].Link != "https://example.com" {
		t.Fatalf("merged states = %+v", got)
	}
}

func TestDynamicLookupsDoNotAllocate(t *testing.T) {
	static := []Feature{{Key: "audit"}}
	d := NewDynamicSet(static)
	g, _ := NewWithDynamic(static, newSource(map[Key]Availability{"audit": {Access: Full}}), d)
	if err := d.Replace([]Feature{{Key: "acme.backup"}, {Key: "acme.logs"}}, newSource(map[Key]Availability{
		"acme.backup": {Access: Full, Limits: []Limit{{Dim: "jobs", Max: 4}}}, "acme.logs": {Access: ReadOnly},
	})); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(1000, func() {
		if _, err := g.Require("audit", true); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Require("acme.backup", true); err != nil {
			t.Fatal(err)
		}
		if err := g.Admit("acme.backup", "jobs", 1, 1); err != nil {
			t.Fatal(err)
		}
		if g.Access("acme.logs") != ReadOnly || !g.Declared("acme.logs") {
			t.Fatal("acme.logs")
		}
	}); n != 0 {
		t.Fatalf("static and dynamic lookups allocate %v times per run", n)
	}
	if n := testing.AllocsPerRun(1000, func() { _, _ = g.Require("acme.logs", true) }); n != 0 {
		t.Fatalf("a dynamic key's prebuilt refusal allocates %v times per run", n)
	}
}

// Lookups run while Replace swaps the set; under -race this proves the
// swap is the only shared write.
func TestReplaceRacesLookups(t *testing.T) {
	d := NewDynamicSet(nil)
	g, _ := NewWithDynamic(nil, nil, d)
	full := newSource(map[Key]Availability{"acme.backup": {Access: Full}})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a, err := g.Require("acme.backup", true)
				if (err == nil) != (a == Full) {
					t.Errorf("Require = %v, %v", a, err)
					return
				}
				_ = g.States()
			}
		}()
	}
	for i := range 2000 {
		var fs []Feature
		if i%2 == 0 {
			fs = []Feature{{Key: "acme.backup"}}
		}
		if err := d.Replace(fs, full); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

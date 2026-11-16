package featuregate

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// snapshotSource answers from an atomically swapped map, the shape a real
// Source has.
type snapshotSource struct {
	snap atomic.Pointer[map[Key]Availability]
}

func newSource(m map[Key]Availability) *snapshotSource {
	s := &snapshotSource{}
	s.snap.Store(&m)
	return s
}

func (s *snapshotSource) Availability(k Key) Availability { return (*s.snap.Load())[k] }

func TestKeyGrammar(t *testing.T) {
	for _, k := range []Key{"ab", "audit", "iam.custom_roles", "app.gpuworks.finetune", "a1_2"} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []Key{"", "a", "Audit", "1abc", "a.b", "iam..roles", "a-b", "one.two.three.four", "x" + Key(strings.Repeat("y", 32)), "audit.", ".audit"} {
		if k.Valid() {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func TestNewRejectsBadDeclarations(t *testing.T) {
	for name, fs := range map[string][]Feature{
		"invalid key":  {{Key: "Bad"}},
		"duplicate":    {{Key: "audit"}, {Key: "audit"}},
		"presentation": {{Key: "audit", Presentation: 7}},
	} {
		if _, err := New(fs, nil); err == nil {
			t.Errorf("%s: New accepted %v", name, fs)
		}
	}
}

func TestAccessAndPresentationWireNames(t *testing.T) {
	got := []string{Hidden.String(), Locked.String(), ReadOnly.String(), Full.String(), Access(9).String(), Promote.String(), Conceal.String()}
	want := []string{"hidden", "locked", "read_only", "full", "hidden", "promote", "hidden"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire names = %v, want %v", got, want)
	}
}

func TestRequireCyclesEveryAccess(t *testing.T) {
	src := newSource(map[Key]Availability{})
	g, err := New([]Feature{{Key: "audit"}, {Key: "iam.sso", Presentation: Conceal}}, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		key       Key
		av        Availability
		write     bool
		access    Access
		code      string
		allowWant bool
	}{
		{"full read", "audit", Availability{Access: Full}, false, Full, "", true},
		{"full write", "audit", Availability{Access: Full}, true, Full, "", true},
		{"read-only read", "audit", Availability{Access: ReadOnly}, false, ReadOnly, "", true},
		{"read-only write", "audit", Availability{Access: ReadOnly}, true, ReadOnly, messages.ErrorFeatureReadOnly, false},
		{"locked read", "audit", Availability{Access: Locked}, false, Locked, messages.ErrorFeatureUnavailable, false},
		{"hidden read", "audit", Availability{Access: Hidden}, false, Hidden, messages.ErrorFeatureUnavailable, false},
		{"concealed locked is hidden", "iam.sso", Availability{Access: Locked}, false, Hidden, messages.ErrorFeatureUnavailable, false},
		{"concealed read-only stays", "iam.sso", Availability{Access: ReadOnly}, false, ReadOnly, "", true},
		{"out of range is hidden", "audit", Availability{Access: 42}, false, Hidden, messages.ErrorFeatureUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src.snap.Store(&map[Key]Availability{tc.key: tc.av})
			access, perr := g.Require(tc.key, tc.write)
			if access != tc.access || (perr == nil) != tc.allowWant {
				t.Fatalf("Require = %v, %v; want %v, allowed=%v", access, perr, tc.access, tc.allowWant)
			}
			if perr != nil {
				if perr.Code() != tc.code || perr.Params()["feature"] != string(tc.key) {
					t.Fatalf("refusal = %s %v, want %s for %s", perr.Code(), perr.Params(), tc.code, tc.key)
				}
			}
			if g.Access(tc.key) != tc.access {
				t.Fatalf("Access = %v, want %v", g.Access(tc.key), tc.access)
			}
		})
	}
}

func TestRequireUsesTheSourcesNotice(t *testing.T) {
	notice := messages.NewProductError("SOME_SOURCE_CODE", "from the source", map[string]any{"until": "x"})
	g, _ := New([]Feature{{Key: "audit"}}, newSource(map[Key]Availability{"audit": {Access: ReadOnly, Notice: notice}}))
	if _, perr := g.Require("audit", false); perr != nil {
		t.Fatalf("read refused: %v", perr)
	}
	if _, perr := g.Require("audit", true); perr != notice {
		t.Fatalf("write refusal = %v, want the source's notice", perr)
	}
}

func TestFailsClosed(t *testing.T) {
	g, _ := New([]Feature{{Key: "audit"}, {Key: "infra", Presentation: Conceal}}, nil)
	access, perr := g.Require("audit", false)
	if access != Locked || perr == nil || perr.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("no source: %v %v, want locked FEATURE_PROVIDER_ABSENT", access, perr)
	}
	if access, perr := g.Require("infra", false); access != Hidden || perr.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("no source, concealed: %v %v", access, perr)
	}
	if access, perr := g.Require("undeclared", false); access != Hidden || perr == nil || perr.Code() != messages.ErrorFeatureUnavailable {
		t.Fatalf("undeclared: %v %v", access, perr)
	}
	var nilGate *Gate
	if access, perr := nilGate.Require("audit", false); access != Hidden || perr == nil {
		t.Fatalf("nil gate: %v %v", access, perr)
	}
	if nilGate.Declared("audit") || nilGate.States() != nil || nilGate.Access("audit") != Hidden {
		t.Fatal("nil gate declares nothing")
	}
	if !g.Declared("audit") || g.Declared("undeclared") {
		t.Fatal("Declared is wrong")
	}
}

func TestAdmitLimits(t *testing.T) {
	src := newSource(map[Key]Availability{"pool": {Access: Full, Limits: []Limit{{Dim: "cards", Max: 8}, {Dim: "nodes", Unlimited: true}}}})
	g, _ := New([]Feature{{Key: "pool"}}, src)
	if err := g.Admit("pool", "cards", 6, 2); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	err := g.Admit("pool", "cards", 7, 2)
	if err == nil || err.Code() != messages.ErrorFeatureLimitReached {
		t.Fatalf("past the limit: %v", err)
	}
	if p := err.Params(); p["dim"] != "cards" || p["max"] != int64(8) || p["used"] != int64(7) || p["feature"] != "pool" {
		t.Fatalf("params = %v", p)
	}
	if err := g.Admit("pool", "nodes", 1e6, 1); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	if err := g.Admit("pool", "seats", 1e6, 1); err != nil {
		t.Fatalf("unlisted dimension: %v", err)
	}
	src.snap.Store(&map[Key]Availability{"pool": {Access: ReadOnly}})
	if err := g.Admit("pool", "cards", 0, 1); err == nil || err.Code() != messages.ErrorFeatureReadOnly {
		t.Fatalf("read-only admit: %v", err)
	}
}

func TestStatesAreSortedAndPresented(t *testing.T) {
	src := newSource(map[Key]Availability{"zeta": {Access: Locked, Link: "https://example.invalid/z"}, "alpha": {Access: Full}})
	g, _ := New([]Feature{{Key: "zeta"}, {Key: "alpha", Presentation: Conceal}, {Key: "mid", Presentation: Conceal}}, src)
	got := g.States()
	if len(got) != 3 || got[0].Key != "alpha" || got[1].Key != "mid" || got[2].Key != "zeta" {
		t.Fatalf("order = %+v", got)
	}
	if got[0].Access != Full || got[1].Access != Hidden || got[2].Access != Locked || got[2].Link != "https://example.invalid/z" || got[1].Presentation != Conceal {
		t.Fatalf("states = %+v", got)
	}
}

// TestAllowPathDoesNotAllocate holds the hot-path rule: an extension route's
// gate check, and an admission within its limit, allocate nothing.
func TestAllowPathDoesNotAllocate(t *testing.T) {
	src := newSource(map[Key]Availability{"audit": {Access: Full, Limits: []Limit{{Dim: "cards", Max: 8}}}, "logs": {Access: ReadOnly}})
	g, _ := New([]Feature{{Key: "audit"}, {Key: "logs"}}, src)
	if n := testing.AllocsPerRun(1000, func() {
		if _, err := g.Require("audit", true); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Require("logs", false); err != nil {
			t.Fatal(err)
		}
		if err := g.Admit("audit", "cards", 1, 1); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("allow path allocates %v times per run", n)
	}
	if n := testing.AllocsPerRun(1000, func() { _, _ = g.Require("logs", true) }); n != 0 {
		t.Fatalf("a prebuilt refusal allocates %v times per run", n)
	}
}

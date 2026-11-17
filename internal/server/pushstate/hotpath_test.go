package pushstate

import (
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// hotState is a State with every kind pushed at a realistic size: 1000
// nodes placed for capacity, 2000 explicit holds, 200 feature rows.
func hotState(t testing.TB) *harness {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	nodes := make([]fleetcap.Node, 1000)
	for i := range nodes {
		nodes[i] = fleetcap.Node{ID: uint(i + 1), Cards: 8}
	}
	h.setFleet(nodes...)
	quota := pusherOf(acmeQuota, "push:banners")
	var rows strings.Builder
	rows.WriteString(`{"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"gpus","max":4}]}]}`)
	rows.WriteString(`,{"down_access":"read_only","feature":"acme.reports","steps":[{"access":"locked","notice":{"code":"ACME_GRACE"}}]}]}`)
	h.push2(quota, extapi.KindAvailability, 1, rows.String())
	h.push2(quota, extapi.KindFleetCapacity, 1, capacityBody(900, 8000))
	h.push2(quota, extapi.KindBanners, 1, quotaBanners)
	var holds strings.Builder
	holds.WriteString(`{"nodes":{`)
	for i := 2001; i <= 4000; i++ {
		if i > 2001 {
			holds.WriteByte(',')
		}
		holds.WriteString(`"` + strconv.Itoa(i) + `":{"code":"ACME_MAINTENANCE","params":{"ticket":"T"}}`)
	}
	holds.WriteString(`}}`)
	h.push2(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, holds.String())
	h.selectNow()
	return h
}

func TestHooksDoNotAllocate(t *testing.T) {
	h := hotState(t)
	s := h.s
	// Typed sinks: assigning to an interface would itself allocate.
	var (
		av      featuregate.Availability
		hold    Hold
		banners []Banner
		refusal any
	)
	for name, fn := range map[string]func(){
		"availability, full":             func() { av = s.Availability("acme.backup") },
		"availability, locked":           func() { av = s.Availability("acme.reports") },
		"availability, absent":           func() { av = s.Availability("acme.unknown") },
		"guard, room left":               func() { refusal = s.GuardNode(10, 80) },
		"guard, refused":                 func() { refusal = s.GuardNode(900, 80) },
		"hold, kept node":                func() { hold = s.Hold(5, 8) },
		"hold, capacity-held node":       func() { hold = s.Hold(950, 8) },
		"hold, explicit hold":            func() { hold = s.Hold(3000, 8) },
		"hold, unplaced node":            func() { hold = s.Hold(5000, 8) },
		"hold, node whose cards changed": func() { hold = s.Hold(7, 16) },
		"banners, admin":                 func() { banners = s.Banners(true) },
		"banners, member":                func() { banners = s.Banners(false) },
	} {
		if n := testing.AllocsPerRun(1000, fn); n != 0 {
			t.Errorf("%s: %v allocations per call", name, n)
		}
	}
	_, _, _, _ = av, hold, banners, refusal
	if s.Availability("acme.backup").Access != featuregate.Full || !s.Hold(950, 8).held() || !s.Hold(3000, 8).held() || s.Hold(5, 8).held() || s.GuardNode(900, 0) == nil {
		t.Fatal("the hot state is not what the test measures")
	}
}

func BenchmarkHooks(b *testing.B) {
	h := hotState(b)
	s := h.s
	b.Run("availability", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.Availability("acme.backup")
		}
	})
	b.Run("guard", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.GuardNode(10, 80)
		}
	})
	b.Run("hold", func(b *testing.B) {
		b.ReportAllocs()
		id := uint(0)
		for b.Loop() {
			id = id%5000 + 1
			_ = s.Hold(id, 8)
		}
	})
	b.Run("banners", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.Banners(true)
		}
	})
}

// A full replace of a large table writes only the entries that changed.
func TestALargePushWritesOnlyItsChangedEntries(t *testing.T) {
	holds := func(changed map[int]string) *part {
		t := &extapi.NodeHoldsV1{Nodes: map[string]extapi.Notice{}}
		for i := 1; i <= extapi.MaxNodeHolds; i++ {
			code := "ACME_MAINTENANCE"
			if c, ok := changed[i]; ok {
				code = c
			}
			if code != "" {
				t.Nodes[strconv.Itoa(i)] = extapi.Notice{Code: code}
			}
		}
		p := &part{owner: "acme_ops", kind: extapi.KindNodeHolds, schema: 1, gen: 1, table: normalize(t)}
		compile(p)
		return p
	}
	old := holds(nil)
	next := holds(map[int]string{3: "ACME_QUARANTINE", 4: "ACME_QUARANTINE", 9999: ""})
	w := tableWrite(old, next)
	if w.DeleteAll || len(w.DeleteEntries) != 3 || len(w.Insert.NodeHolds) != 2 {
		t.Fatalf("diff deletes %v (all %v) and inserts %d rows", w.DeleteEntries, w.DeleteAll, len(w.Insert.NodeHolds))
	}
	if first := tableWrite(nil, next); !first.DeleteAll || len(first.Insert.NodeHolds) != extapi.MaxNodeHolds-1 {
		t.Fatal("a first push does not write the whole table")
	}
}

// BenchmarkCommitLargeHolds measures one writer tick that replaces a
// 10 000-node holds table with ten entries changed.
func BenchmarkCommitLargeHolds(b *testing.B) {
	h := newHarness(b, openStore(b, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	ops := pusherOf(acmeOps, "push:node_holds")
	body := func(variant int) string {
		var s strings.Builder
		s.WriteString(`{"nodes":{`)
		for i := 1; i <= extapi.MaxNodeHolds; i++ {
			if i > 1 {
				s.WriteByte(',')
			}
			code := "ACME_MAINTENANCE"
			if i <= 10 && variant%2 == 1 {
				code = "ACME_QUARANTINE"
			}
			s.WriteString(`"` + strconv.Itoa(i) + `":{"code":"` + code + `"}`)
		}
		s.WriteString(`}}`)
		return s.String()
	}
	h.push2(ops, extapi.KindNodeHolds, 1, body(0))
	gen := uint64(1)
	for b.Loop() {
		gen++
		h.push2(ops, extapi.KindNodeHolds, gen, body(int(gen)))
	}
}

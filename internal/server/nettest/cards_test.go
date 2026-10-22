package nettest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// cardAgents answers accelerator.ping the way agents do: one answer per
// probe in request order, cards of a node side by side, and an address that
// does not answer reported as unreachable. It records what ran together.
type cardAgents struct {
	mu       sync.Mutex
	owner    map[string]string // card address -> "node/card"
	reach    func(source, target string) bool
	rpcErr   map[string]error                                   // node -> the RPC itself fails
	answer   map[string]func([]proto.AcceleratorPingResult) any // node -> rewrite the answer
	toolFail map[string]string                                  // "node/card" -> tool error
	noCard   map[string]bool                                    // "node/card" the node does not have
	inFlight map[string]int                                     // "node/card" pings running
	overlap  []string
	busiest  int // most source cards pinging at once
	calls    int
	probes   int
	timeouts map[int]bool
	delay    time.Duration
}

func (f *cardAgents) call(_ context.Context, node, method string, params any) (json.RawMessage, error) {
	if method != proto.RPCAcceleratorPing {
		return nil, fmt.Errorf("unexpected RPC %s", method)
	}
	raw, _ := json.Marshal(params)
	var p proto.AcceleratorPingParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls++
	f.probes += len(p.Probes)
	if f.timeouts == nil {
		f.timeouts = map[int]bool{}
	}
	f.timeouts[p.TimeoutMS] = true
	if err := f.rpcErr[node]; err != nil {
		f.mu.Unlock()
		return nil, err
	}
	if f.inFlight == nil {
		f.inFlight = map[string]int{}
	}
	seen := map[string]bool{}
	for _, probe := range p.Probes {
		key := node + "/" + probe.Card
		if seen[key] {
			f.overlap = append(f.overlap, key+" twice in one call")
		}
		seen[key] = true
		f.inFlight[key]++
		if f.inFlight[key] > 1 {
			f.overlap = append(f.overlap, key)
		}
	}
	busy := 0
	for _, n := range f.inFlight {
		if n > 0 {
			busy++
		}
	}
	f.busiest = max(f.busiest, busy)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proto.AcceleratorPingResult, len(p.Probes))
	for i, probe := range p.Probes {
		key := node + "/" + probe.Card
		f.inFlight[key]--
		out[i] = proto.AcceleratorPingResult{Card: probe.Card, Target: probe.Target}
		switch {
		case f.noCard[key]:
			out[i].Code, out[i].Error = proto.AcceleratorPingNoCard, "the node reports no card named "+probe.Card
		case f.toolFail[key] != "":
			out[i].Code, out[i].Error = proto.AcceleratorPingToolFailed, f.toolFail[key]
		case f.reach(key, f.owner[probe.Target]):
			out[i].Reachable, out[i].RTTMS = true, 0.25
		default:
			out[i].Code, out[i].Error = proto.AcceleratorPingUnreachable, "no answer within the time limit"
		}
	}
	if rewrite := f.answer[node]; rewrite != nil {
		return json.Marshal(rewrite(out))
	}
	return json.Marshal(out)
}

// cardEndpoints builds nodes n0..n(N-1), each with the given cards; card c of
// node i has address 172.20.i.c.
func cardEndpoints(f *cardAgents, nodes int, cards ...int) []Endpoint {
	f.owner = map[string]string{}
	out := make([]Endpoint, nodes)
	for i := range out {
		uuid := fmt.Sprintf("n%02d", i)
		out[i] = Endpoint{NodeUUID: uuid, IP: fmt.Sprintf("10.0.0.%d", i+1)}
		for _, c := range cards {
			addr := fmt.Sprintf("172.20.%d.%d", i, c)
			out[i].Rails = append(out[i].Rails, Rail{Name: fmt.Sprintf("npu%d", c), Addr: addr + "/16"})
			f.owner[addr] = fmt.Sprintf("%s/npu%d", uuid, c)
		}
	}
	return out
}

func cardOf(key string) string {
	for i := range key {
		if key[i] == '/' {
			return key[i+1:]
		}
	}
	return key
}

// sameCard is two hosts cabled card to card; anyCard a switched fabric.
func sameCard(source, target string) bool { return cardOf(source) == cardOf(target) }
func anyCard(string, string) bool         { return true }

func TestCardEstimate(t *testing.T) {
	for _, tc := range []struct {
		mode, pattern        string
		nodes, cards         int
		pairs, rounds, pings int
	}{
		// The four combinations of 24 nodes with 8 cards.
		{ModeCardPing, PatternRing, 24, 8, 24, 1, 192},
		{ModeCardMatrix, PatternRing, 24, 8, 24, 8, 1536},
		{ModeCardPing, PatternMesh, 24, 8, 276, 12, 2208},
		{ModeCardMatrix, PatternMesh, 24, 8, 276, 96, 17664},
		{ModeCardMatrix, PatternMesh, 16, 8, 120, 64, 7680},
		// Two nodes are one pair either way; an odd mesh has no half round.
		{ModeCardMatrix, PatternRing, 2, 8, 1, 8, 64},
		{ModeCardMatrix, PatternMesh, 2, 8, 1, 8, 64},
		{ModeCardPing, PatternMesh, 5, 8, 10, 2, 80},
		{ModeCardPing, PatternMesh, 3, 2, 3, 1, 6},
		{ModeCardPing, PatternMesh, 1, 8, 0, 0, 0},
		{ModeCardMatrix, PatternRing, 0, 8, 0, 0, 0},
	} {
		cards := make([]int, tc.cards)
		for i := range cards {
			cards[i] = i
		}
		endpoints := cardEndpoints(&cardAgents{}, tc.nodes, cards...)
		got := EstimateCards(endpoints, Options{Mode: tc.mode, Pattern: tc.pattern})
		if want := (CardEstimate{Pairs: tc.pairs, Rounds: tc.rounds, Probes: tc.pings}); got != want {
			t.Errorf("%s %s %dx%d: estimate = %+v, want %+v", tc.mode, tc.pattern, tc.nodes, tc.cards, got, want)
		}
		if got := CardPairCount(tc.pattern, tc.nodes); got != tc.pairs {
			t.Errorf("%s %d nodes: pair count = %d, want %d", tc.pattern, tc.nodes, got, tc.pairs)
		}
	}
	// Nodes that cannot take part cost no round, and a card without an
	// address no probe: two hosts of 2 and 4 cards (one unaddressed), plus
	// an offline one in the ring.
	f := &cardAgents{}
	endpoints := cardEndpoints(f, 3, 3, 4)
	endpoints[1].Rails = append(endpoints[1].Rails, Rail{Name: "npu5", Addr: "172.20.1.5/16"}, Rail{Name: "npu6"})
	endpoints[2].Unavailable = CodeOffline
	if got, want := EstimateCards(endpoints, Options{Mode: ModeCardMatrix, Pattern: PatternRing}), (CardEstimate{Pairs: 3, Rounds: 4, Probes: 6}); got != want {
		t.Errorf("uneven estimate = %+v, want %+v", got, want)
	}
}

// Every unordered node pair is probed exactly once, and the run sends what
// the counts promise, with no card pinging twice at once.
func TestCardRunCoversEveryPairOnce(t *testing.T) {
	for _, tc := range []struct {
		mode, pattern string
		nodes         int
	}{
		{ModeCardMatrix, PatternMesh, 6}, {ModeCardMatrix, PatternMesh, 5}, {ModeCardMatrix, PatternRing, 6},
		{ModeCardPing, PatternMesh, 6}, {ModeCardPing, PatternRing, 5}, {ModeCardMatrix, PatternMesh, 2}, {ModeCardPing, PatternRing, 2},
	} {
		f := &cardAgents{reach: anyCard, delay: 2 * time.Millisecond}
		endpoints := cardEndpoints(f, tc.nodes, 0, 1, 2, 3)
		pairs := Run(context.Background(), f.call, endpoints, Options{Mode: tc.mode, Pattern: tc.pattern})
		name := fmt.Sprintf("%s %s %d nodes", tc.mode, tc.pattern, tc.nodes)
		if len(pairs) != CardPairCount(tc.pattern, tc.nodes) {
			t.Fatalf("%s: pairs = %d, want %d", name, len(pairs), CardPairCount(tc.pattern, tc.nodes))
		}
		seen := map[string]bool{}
		perPair := 4
		if tc.mode == ModeCardMatrix {
			perPair = 16
		}
		for _, p := range pairs {
			a, b := p.SourceUUID, p.TargetUUID
			if a > b {
				a, b = b, a
			}
			if seen[a+b] || a == b {
				t.Fatalf("%s: node pair %s-%s probed twice or with itself", name, a, b)
			}
			seen[a+b] = true
			if len(p.Rails) != perPair || p.Status(false) != StatusPassed || !p.Reachable {
				t.Fatalf("%s: pair %s->%s = %d card pairs, status %s", name, p.SourceUUID, p.TargetUUID, len(p.Rails), p.Status(false))
			}
			cards := map[string]bool{}
			for _, r := range p.Rails {
				if cards[r.Rail+">"+r.TargetRail] || !r.Reachable || r.RTTMS != 0.25 || r.SourceIP == "" || r.TargetIP == "" {
					t.Fatalf("%s: card pair %+v", name, r)
				}
				cards[r.Rail+">"+r.TargetRail] = true
			}
		}
		// What was estimated is what ran.
		estimate := EstimateCards(endpoints, Options{Mode: tc.mode, Pattern: tc.pattern})
		if f.probes != estimate.Probes || len(pairs) != estimate.Pairs {
			t.Fatalf("%s: ran %d probes over %d pairs, estimated %+v", name, f.probes, len(pairs), estimate)
		}
		if len(f.overlap) > 0 {
			t.Fatalf("%s: cards pinged twice at once: %v", name, f.overlap)
		}
		if !reflect.DeepEqual(f.timeouts, map[int]bool{3000: true}) {
			t.Fatalf("%s: probe time limits = %v", name, f.timeouts)
		}
		// A full round has every source card of the round's nodes busy.
		if tc.nodes == 6 && tc.pattern == PatternMesh && f.busiest != 24 {
			t.Fatalf("%s: most cards pinging at once = %d, want all 24", name, f.busiest)
		}
	}
}

// Two hosts cabled card to card: the matrix is a diagonal, which the
// addresses (one subnet for all cards) do not show.
func TestCardMatrixFindsTheDiagonal(t *testing.T) {
	f := &cardAgents{reach: sameCard}
	endpoints := cardEndpoints(f, 2, 3, 4)
	// The second host has more cards, one of them without an address.
	endpoints[1].Rails = append(endpoints[1].Rails, Rail{Name: "npu5", Addr: "172.20.1.5/16"}, Rail{Name: "npu6"})
	f.owner["172.20.1.5"] = "n01/npu5"
	pairs := Run(context.Background(), f.call, endpoints, Options{Mode: ModeCardMatrix, Pattern: PatternMesh})
	if len(pairs) != 1 || pairs[0].SourceUUID != "n00" || pairs[0].TargetUUID != "n01" || pairs[0].Target != "10.0.0.2" {
		t.Fatalf("pairs = %+v", pairs)
	}
	got := map[string]string{}
	var order []string
	for _, r := range pairs[0].Rails {
		key := r.Rail + ">" + r.TargetRail
		order = append(order, key)
		switch {
		case r.OK():
			got[key] = "ok"
		default:
			got[key] = r.Code + "/" + r.Side
		}
	}
	want := map[string]string{
		"npu3>npu3": "ok", "npu3>npu4": "unreachable/", "npu3>npu5": "unreachable/", "npu3>npu6": "no_address/target",
		"npu4>npu3": "unreachable/", "npu4>npu4": "ok", "npu4>npu5": "unreachable/", "npu4>npu6": "no_address/target",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix = %v\nwant %v", got, want)
	}
	if wantOrder := []string{"npu3>npu3", "npu3>npu4", "npu3>npu5", "npu3>npu6", "npu4>npu3", "npu4>npu4", "npu4>npu5", "npu4>npu6"}; !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %v", order)
	}
	if pairs[0].Status(false) != StatusPartial {
		t.Fatalf("status = %s", pairs[0].Status(false))
	}
	// Four shifts (the target's cards), two probes each, the one without an
	// address not sent.
	if f.calls != 4 || f.probes != 6 {
		t.Fatalf("calls = %d, probes = %d", f.calls, f.probes)
	}
	// The stored form of one reachable and one unreachable card pair.
	raw, _ := json.Marshal(pairs[0].Rails[:2])
	if want := `[{"rail":"npu3","source_ip":"172.20.0.3","target_rail":"npu3","target_ip":"172.20.1.3","gbps":0,"reachable":true,"rtt_ms":0.25},` +
		`{"rail":"npu3","source_ip":"172.20.0.3","target_rail":"npu4","target_ip":"172.20.1.4","gbps":0,"code":"unreachable","error":"no answer within the time limit"}]`; string(raw) != want {
		t.Fatalf("stored = %s\nwant    %s", raw, want)
	}
}

// Same-named cards pair up; a card without a namesake is reported on its side.
func TestCardPingPairsCardsByName(t *testing.T) {
	f := &cardAgents{reach: sameCard}
	endpoints := cardEndpoints(f, 2, 3, 4)
	endpoints[0].Rails = append(endpoints[0].Rails, Rail{Name: "npu7", Addr: "172.20.0.7/16"})
	endpoints[1].Rails = append([]Rail{{Name: "npu0", Addr: "172.20.1.0/16"}}, endpoints[1].Rails...)
	pairs := Run(context.Background(), f.call, endpoints, Options{Mode: ModeCardPing, Pattern: PatternRing})
	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v", pairs)
	}
	var got []string
	for _, r := range pairs[0].Rails {
		got = append(got, fmt.Sprintf("%s>%s:%s/%s:%v", r.Rail, r.TargetRail, r.Code, r.Side, r.OK()))
	}
	want := []string{"npu3>npu3:/:true", "npu4>npu4:/:true", "npu7>:no_peer_rail/source:false", ">npu0:no_peer_rail/target:false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("card pairs = %v\nwant %v", got, want)
	}
	if f.calls != 1 || f.probes != 2 || pairs[0].Status(false) != StatusPartial {
		t.Fatalf("calls = %d, probes = %d, status = %s", f.calls, f.probes, pairs[0].Status(false))
	}
}

func TestCardRunFailures(t *testing.T) {
	f := &cardAgents{
		reach:    anyCard,
		rpcErr:   map[string]error{"n01": errors.New("unknown method accelerator.ping")},
		toolFail: map[string]string{"n02/npu4": "Command execute failed!"},
		noCard:   map[string]bool{"n02/npu3": true},
		answer: map[string]func([]proto.AcceleratorPingResult) any{
			"n03": func(out []proto.AcceleratorPingResult) any { return out[:1] },
			"n07": func([]proto.AcceleratorPingResult) any { return "not a list" },
		},
	}
	endpoints := cardEndpoints(f, 8, 3, 4)
	endpoints[5].Unavailable = CodeOffline
	endpoints[6].Rails = nil
	pairs := Run(context.Background(), f.call, endpoints, Options{Mode: ModeCardPing, Pattern: PatternRing})
	by := map[string]Pair{}
	for _, p := range pairs {
		by[p.SourceUUID] = p
	}
	rails := func(uuid string) []string {
		var out []string
		for _, r := range by[uuid].Rails {
			out = append(out, fmt.Sprintf("%s:%s/%s:%s", r.Rail, r.Code, r.Side, r.Error))
		}
		return out
	}
	// n00 probes n01 and works; the next probe does not depend on it.
	if p := by["n00"]; p.Status(false) != StatusPassed {
		t.Fatalf("n00 = %+v", p)
	}
	// An agent that does not know the RPC fails every card pair it sources.
	if got, want := rails("n01"), []string{"npu3:rpc_failed/source:unknown method accelerator.ping", "npu4:rpc_failed/source:unknown method accelerator.ping"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("n01 = %v", got)
	}
	if by["n01"].Status(false) != StatusFailed {
		t.Fatalf("n01 status = %s", by["n01"].Status(false))
	}
	// The tool's own failure and a card the node lost are told apart.
	if got, want := rails("n02"), []string{"npu3:no_card/source:the node reports no card named npu3", "npu4:tool_failed/source:Command execute failed!"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("n02 = %v", got)
	}
	// A short or unreadable answer is an RPC failure, not a guess.
	for _, uuid := range []string{"n03", "n07"} {
		for _, r := range by[uuid].Rails {
			if r.Code != CodeRPCFailed || r.Side != SideSource || r.Error == "" {
				t.Fatalf("%s rail = %+v", uuid, r)
			}
		}
		if len(by[uuid].Rails) != 2 {
			t.Fatalf("%s rails = %+v", uuid, by[uuid].Rails)
		}
	}
	// A node that is offline or has no card on the network fails the pairs
	// it is in without a probe, and the reason names its side.
	for uuid, want := range map[string][2]string{
		"n04": {CodeOffline, SideTarget}, "n05": {CodeOffline, SideSource}, "n06": {CodeNoHCA, SideSource},
	} {
		if p := by[uuid]; p.Code != want[0] || p.Side != want[1] || len(p.Rails) != 0 || p.Status(false) != StatusFailed {
			t.Fatalf("%s = %s/%s with %d card pairs, want %v", uuid, p.Code, p.Side, len(p.Rails), want)
		}
	}
	if len(f.overlap) > 0 {
		t.Fatalf("overlap = %v", f.overlap)
	}
}

// A round lasts as long as its slowest probe, whatever the node count.
func TestCardRunTimeFollowsRoundsNotProbes(t *testing.T) {
	f := &cardAgents{reach: sameCard, delay: 20 * time.Millisecond}
	endpoints := cardEndpoints(f, 8, 0, 1, 2, 3)
	start := time.Now()
	pairs := Run(context.Background(), f.call, endpoints, Options{Mode: ModeCardMatrix, Pattern: PatternMesh})
	took := time.Since(start)
	rounds := EstimateCards(endpoints, Options{Mode: ModeCardMatrix, Pattern: PatternMesh}).Rounds
	if rounds != 16 || len(pairs) != 28 || f.probes != 448 {
		t.Fatalf("rounds = %d, pairs = %d, probes = %d", rounds, len(pairs), f.probes)
	}
	if took < time.Duration(rounds)*f.delay || took > time.Duration(rounds)*f.delay*3 {
		t.Fatalf("448 probes in %d rounds took %v, want about %v", rounds, took, time.Duration(rounds)*f.delay)
	}
	if len(f.overlap) > 0 {
		t.Fatalf("overlap = %v", f.overlap)
	}
}

func shapeRail(source, target string, ok bool) RailResult {
	r := RailResult{Rail: source, TargetRail: target}
	if ok {
		r.Reachable, r.RTTMS = true, 0.2
	} else {
		r.Code, r.Error = CodeUnreachable, "no answer within the time limit"
	}
	return r
}

func shapePair(reach func(a, b string) bool) Pair {
	p := Pair{SourceUUID: "a", TargetUUID: "b", Reachable: true}
	for _, a := range []string{"npu3", "npu4"} {
		for _, b := range []string{"npu3", "npu4"} {
			p.Rails = append(p.Rails, shapeRail(a, b, reach(a, b)))
		}
	}
	return p
}

func TestCardShapeAndStatus(t *testing.T) {
	lonely := RailResult{Rail: "npu7", Code: CodeNoPeerRail, Side: SideSource}
	for name, tc := range map[string]struct {
		pair          Pair
		shape, status string
	}{
		"every pair":            {shapePair(func(a, b string) bool { return true }), ShapeFull, StatusPassed},
		"same-named only":       {shapePair(func(a, b string) bool { return a == b }), ShapeDiagonal, StatusPassed},
		"nothing":               {shapePair(func(a, b string) bool { return false }), ShapeNone, StatusFailed},
		"cross-wired":           {shapePair(func(a, b string) bool { return a != b }), ShapeMixed, StatusPartial},
		"one same-named down":   {shapePair(func(a, b string) bool { return a == b && a == "npu3" }), ShapeMixed, StatusPartial},
		"diagonal plus one":     {shapePair(func(a, b string) bool { return a == b || (a == "npu3" && b == "npu4") }), ShapeMixed, StatusPartial},
		"node offline":          {Pair{Code: CodeOffline, Side: SideTarget}, ShapeUntested, StatusFailed},
		"no card had a peer":    {Pair{Reachable: true, Rails: []RailResult{lonely}}, ShapeUntested, StatusFailed},
		"same-named, all reach": {Pair{Reachable: true, Rails: []RailResult{shapeRail("npu3", "npu3", true), shapeRail("npu4", "npu4", true), lonely}}, ShapeFull, StatusPassed},
		"same-named, one fails": {Pair{Reachable: true, Rails: []RailResult{shapeRail("npu3", "npu3", true), shapeRail("npu4", "npu4", false)}}, ShapeMixed, StatusPartial},
	} {
		if got := CardShape(tc.pair); got != tc.shape {
			t.Errorf("%s: shape = %s, want %s", name, got, tc.shape)
		}
		if got := PairStatus(ModeCardMatrix, tc.pair, false); got != tc.status {
			t.Errorf("%s: status = %s, want %s", name, got, tc.status)
		}
	}
	// Outside the card modes the status is the pair's own.
	tcp := Pair{Reachable: true, Gbps: 9}
	if PairStatus(ModeTCP, tcp, true) != StatusPassed || PairStatus("", Pair{Code: CodeOffline}, false) != StatusFailed {
		t.Fatal("non-card status changed")
	}
}

func TestCardVerdictAndCounts(t *testing.T) {
	diagonal := shapePair(func(a, b string) bool { return a == b })
	none := shapePair(func(a, b string) bool { return false })
	full := shapePair(func(a, b string) bool { return true })
	if got := CardVerdict([]Pair{diagonal, diagonal, none}); got != ShapeDiagonal {
		t.Fatalf("verdict = %s", got)
	}
	// A tie goes to the healthier shape.
	if got := CardVerdict([]Pair{none, full}); got != ShapeFull {
		t.Fatalf("tie verdict = %s", got)
	}
	if got := CardVerdict(nil); got != "" {
		t.Fatalf("empty verdict = %q", got)
	}
	if got := CardVerdict([]Pair{{Code: CodeOffline}}); got != ShapeUntested {
		t.Fatalf("untested verdict = %s", got)
	}
	// Only pinged pairs count: a card without a namesake is not a failure.
	lonely := Pair{Reachable: true, Rails: []RailResult{shapeRail("npu3", "npu3", true), {Rail: "npu7", Code: CodeNoPeerRail, Side: SideSource}}}
	if probed, reachable := CardCounts([]Pair{diagonal, lonely, {Code: CodeOffline}}); probed != 5 || reachable != 3 {
		t.Fatalf("counts = %d/%d, want 3/5", reachable, probed)
	}
}

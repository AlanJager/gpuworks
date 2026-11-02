package nettest

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// An accelerator network's rails are the cards' own ports, which only
// the node's vendor tool can ping. Which card reaches which is a wiring fact
// the addresses do not show (two hosts cabled card to card keep every card in
// one subnet), so it is measured: a card of one node pings a card of the
// other.
//
// What bounds a run is its rounds, not its probes. A card sends one ping at a
// time (two at once leave it failing for a while) and an address that does
// not answer costs the whole time limit, so a round takes that long at most
// whatever the number of nodes and cards. Node pairs rotate as in Run: in
// node round k node i probes node i+k. Within it cards rotate the same way:
// in shift s card c of the source pings card c+s of the target, so every
// card is a source once and a target once per round.

// Card test modes.
const (
	// ModeCardPing pings between the cards two nodes name alike (npu3 to
	// npu3): one shift.
	ModeCardPing = "card_ping"
	// ModeCardMatrix pings from every card of a node to every card of the
	// other: as many shifts as the target has cards.
	ModeCardMatrix = "card_matrix"
)

// CardProbeTimeout is how long one ping may wait for an answer. A reachable
// port answers in one to two seconds (the tool reports three packets at once).
const CardProbeTimeout = 3 * time.Second

// Why a card pair failed, beside the codes pairs share.
const (
	CodeUnreachable = proto.AcceleratorPingUnreachable // nothing answered within the time limit
	CodeNoCard      = proto.AcceleratorPingNoCard      // the source node does not have the card
)

// CardMode reports whether mode is one of the card test modes.
func CardMode(mode string) bool { return mode == ModeCardPing || mode == ModeCardMatrix }

// cardNodeRounds is how many node rounds a card run of n nodes takes. A ping
// is answered, so one probe proves both directions and an unordered pair is
// probed once: a mesh needs rounds 1..n/2 (in the last one of an even n only
// the first half probes), a ring round 1.
func cardNodeRounds(pattern string, n int) int {
	switch {
	case n < 2:
		return 0
	case pattern == PatternRing:
		return 1
	}
	return n / 2
}

// cardRoundPairs lists the node pairs (source, target indexes) of node round k.
func cardRoundPairs(n, k int) [][2]int {
	var out [][2]int
	for i := 0; i < n; i++ {
		if 2*k == n && i >= n/2 {
			break // the second half would repeat the first half's pairs
		}
		out = append(out, [2]int{i, (i + k) % n})
	}
	return out
}

// CardPairCount is how many node pairs a card run of n nodes probes.
func CardPairCount(pattern string, n int) int {
	count := 0
	for k := 1; k <= cardNodeRounds(pattern, n); k++ {
		count += len(cardRoundPairs(n, k))
	}
	return count
}

// cardProbe is one ping of a pair: the source rail and the target rail.
type cardProbe struct {
	source, target Rail
}

// cardShift lists the pings of one shift between two nodes: every source
// card to one target card, no source card twice.
func cardShift(mode string, source, target []Rail, shift int) []cardProbe {
	var out []cardProbe
	if mode == ModeCardMatrix {
		if shift >= len(target) {
			return nil
		}
		for i, s := range source {
			out = append(out, cardProbe{source: s, target: target[(i+shift)%len(target)]})
		}
		return out
	}
	if shift > 0 {
		return nil
	}
	for _, s := range source {
		for _, t := range target {
			if s.Name == t.Name {
				out = append(out, cardProbe{source: s, target: t})
			}
		}
	}
	return out
}

// cardStep is one node pair's pings in one round: what its source node is
// asked to send at once.
type cardStep struct {
	pair   int // index into the plan's pairs
	source string
	probes []cardProbe
}

// cardPlan lays a run out before anything is sent: one Pair per node pair
// (already failed when a node cannot take part), and the rounds in order.
// Within a round every step has a different source node and no source card
// appears twice. Estimating a run and running it both read this plan, so what
// the form promises is what runs.
func cardPlan(endpoints []Endpoint, opts Options) (pairs []Pair, ends [][2]Endpoint, rounds [][]cardStep) {
	n := len(endpoints)
	for k := 1; k <= cardNodeRounds(opts.Pattern, n); k++ {
		first := len(pairs)
		shifts := 0
		for _, ij := range cardRoundPairs(n, k) {
			source, target := endpoints[ij[0]], endpoints[ij[1]]
			pair := Pair{SourceUUID: source.NodeUUID, TargetUUID: target.NodeUUID, Target: target.IP}
			switch {
			case pair.unavailable(source, target):
			case len(source.Rails) == 0:
				pair.fail(CodeNoHCA, SideSource, "")
			case len(target.Rails) == 0:
				pair.fail(CodeNoHCA, SideTarget, "")
			default:
				pair.Reachable = true
				if opts.Mode == ModeCardMatrix {
					shifts = max(shifts, len(target.Rails))
				} else {
					shifts = max(shifts, 1)
				}
			}
			pairs = append(pairs, pair)
			ends = append(ends, [2]Endpoint{source, target})
		}
		for shift := 0; shift < shifts; shift++ {
			var round []cardStep
			for p := first; p < len(pairs); p++ {
				if pairs[p].Code != "" {
					continue
				}
				if probes := cardShift(opts.Mode, ends[p][0].Rails, ends[p][1].Rails, shift); len(probes) > 0 {
					round = append(round, cardStep{pair: p, source: ends[p][0].NodeUUID, probes: probes})
				}
			}
			if len(round) > 0 {
				rounds = append(rounds, round)
			}
		}
	}
	return pairs, ends, rounds
}

// CardEstimate is the size of a card run before it starts. The run lasts
// Rounds times CardProbeTimeout at most.
type CardEstimate struct {
	Pairs  int // node pairs
	Rounds int
	Probes int // pings sent
}

// EstimateCards sizes the run runCards would do over these endpoints.
func EstimateCards(endpoints []Endpoint, opts Options) CardEstimate {
	pairs, _, rounds := cardPlan(endpoints, opts)
	out := CardEstimate{Pairs: len(pairs), Rounds: len(rounds)}
	for _, round := range rounds {
		for _, step := range round {
			for _, probe := range step.probes {
				if probe.target.ip() != "" {
					out.Probes++
				}
			}
		}
	}
	return out
}

// runCards probes the card pairs of a run and returns one Pair per node pair,
// sorted by source, target; each Pair's Rails are its card pairs in source
// then target card order.
func runCards(ctx context.Context, call Caller, endpoints []Endpoint, opts Options) []Pair {
	pairs, ends, rounds := cardPlan(endpoints, opts)
	for _, round := range rounds {
		var wg sync.WaitGroup
		for _, step := range round {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Every step of a round is another pair, so these
				// appends never touch the same Pair.
				pairs[step.pair].Rails = append(pairs[step.pair].Rails, cardPing(ctx, call, step.source, opts.Protocol, step.probes)...)
			}()
		}
		wg.Wait()
	}
	for p := range pairs {
		if pairs[p].Code == "" {
			finishCardPair(&pairs[p], opts.Mode, ends[p][0].Rails, ends[p][1].Rails)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].SourceUUID != pairs[j].SourceUUID {
			return pairs[i].SourceUUID < pairs[j].SourceUUID
		}
		return pairs[i].TargetUUID < pairs[j].TargetUUID
	})
	return pairs
}

// cardPing sends one shift's pings from a node and turns the answers into
// rail results. A probe whose target card has no address is not sent.
func cardPing(ctx context.Context, call Caller, sourceUUID, protocol string, probes []cardProbe) []RailResult {
	results := make([]RailResult, len(probes))
	params := proto.AcceleratorPingParams{TimeoutMS: int(CardProbeTimeout / time.Millisecond), Protocol: protocol}
	var sent []int
	for i, p := range probes {
		results[i] = RailResult{Rail: p.source.Name, SourceIP: p.source.ip(), TargetRail: p.target.Name, TargetIP: p.target.ip()}
		if p.target.ip() == "" {
			results[i].Code, results[i].Side = CodeNoAddress, SideTarget
			results[i].Error = fallback(CodeNoAddress, SideTarget, "")
			continue
		}
		params.Probes = append(params.Probes, proto.AcceleratorPingProbe{Card: p.source.Name, Target: p.target.ip()})
		sent = append(sent, i)
	}
	if len(sent) == 0 {
		return results
	}
	rctx, cancel := context.WithTimeout(ctx, CardProbeTimeout+15*time.Second)
	defer cancel()
	raw, err := call(rctx, sourceUUID, proto.RPCAcceleratorPing, params)
	var answers []proto.AcceleratorPingResult
	if err == nil {
		if decodeErr := json.Unmarshal(raw, &answers); decodeErr != nil {
			err = decodeErr
		} else if len(answers) != len(sent) {
			err = errAnswerCount
		}
	}
	for at, i := range sent {
		if err != nil {
			results[i].Code, results[i].Side, results[i].Error = CodeRPCFailed, SideSource, err.Error()
			continue
		}
		answer := answers[at]
		switch {
		case answer.Reachable:
			results[i].Reachable, results[i].RTTMS = true, answer.RTTMS
		case answer.Code == CodeUnreachable:
			results[i].Code, results[i].Error = CodeUnreachable, fallback(CodeUnreachable, "", "")
		case answer.Code == CodeNoCard:
			results[i].Code, results[i].Side, results[i].Error = CodeNoCard, SideSource, answer.Error
		default:
			results[i].Code, results[i].Side, results[i].Error = CodeToolFailed, SideSource, answer.Error
		}
	}
	return results
}

var errAnswerCount = errors.New("the node answered a different number of probes than it was sent")

// finishCardPair orders a pair's card results and, in ModeCardPing, adds the
// cards that have no namesake on the other node.
func finishCardPair(pair *Pair, mode string, source, target []Rail) {
	if mode == ModeCardPing {
		named := func(rails []Rail, name string) bool {
			for _, r := range rails {
				if r.Name == name {
					return true
				}
			}
			return false
		}
		for _, s := range source {
			if !named(target, s.Name) {
				pair.Rails = append(pair.Rails, RailResult{Rail: s.Name, SourceIP: s.ip(), Code: CodeNoPeerRail, Side: SideSource, Error: fallback(CodeNoPeerRail, SideSource, "")})
			}
		}
		for _, t := range target {
			if !named(source, t.Name) {
				pair.Rails = append(pair.Rails, RailResult{TargetRail: t.Name, TargetIP: t.ip(), Code: CodeNoPeerRail, Side: SideTarget, Error: fallback(CodeNoPeerRail, SideTarget, "")})
			}
		}
	}
	position := func(rails []Rail, name string) int {
		for i, r := range rails {
			if r.Name == name {
				return i
			}
		}
		return len(rails)
	}
	sort.SliceStable(pair.Rails, func(i, j int) bool {
		a, b := pair.Rails[i], pair.Rails[j]
		if pa, pb := position(source, a.Rail), position(source, b.Rail); pa != pb {
			return pa < pb
		}
		return position(target, a.TargetRail) < position(target, b.TargetRail)
	})
}

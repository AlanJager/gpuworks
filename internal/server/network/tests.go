package network

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/netplan"

	"gpuworks.ai/gpuworks/internal/server/nettest"
)

// A network's internal test is run by nettest; these are the pieces the API
// needs to accept a request and read a stored run, so it asks this service
// alone about networks.

// Test patterns a request may name.
const (
	TestPatternMesh = nettest.PatternMesh
	TestPatternRing = nettest.PatternRing
)

// Pair outcomes of a stored run.
const (
	TestPassed  = nettest.StatusPassed
	TestPartial = nettest.StatusPartial
	TestFailed  = nettest.StatusFailed
)

// Card test kinds: pings between the ports of accelerator cards.
const (
	TestCardPing   = nettest.ModeCardPing
	TestCardMatrix = nettest.ModeCardMatrix
)

// Shapes a card test reads between two nodes.
const (
	CardShapeFull     = nettest.ShapeFull
	CardShapeDiagonal = nettest.ShapeDiagonal
	CardShapeMixed    = nettest.ShapeMixed
	CardShapeNone     = nettest.ShapeNone
	CardShapeUntested = nettest.ShapeUntested
)

// CardTestMaxSeconds bounds a card test: its rounds times the time limit of
// one probe may not exceed it. A card sends one ping at a time and an
// address that does not answer costs the whole limit, so the rounds, not the
// number of probes or nodes, are what a run takes.
const CardTestMaxSeconds = 600

// CardTest reports whether kind is a card test kind.
func CardTest(kind string) bool { return nettest.CardMode(kind) }

// TestPairCount is how many pairs a run of n nodes with the kind and pattern
// probes.
func TestPairCount(kind, pattern string, n int) int {
	if nettest.CardMode(kind) {
		return nettest.CardPairCount(pattern, n)
	}
	return nettest.PairCount(pattern, n)
}

// TestPairStatus is a pair's outcome in a run of the given kind.
func TestPairStatus(kind string, pair nettest.Pair, bandwidth bool) string {
	return nettest.PairStatus(kind, pair, bandwidth)
}

// CardTestPlan is the size of a card test before it starts.
type CardTestPlan struct {
	Nodes      int
	Pairs      int
	Rounds     int
	Probes     int
	Seconds    int // the longest the run can take
	MaxSeconds int
}

// CardTestTooLongError refuses a card test whose rounds exceed the limit.
type CardTestTooLongError struct{ Plan CardTestPlan }

func (e *CardTestTooLongError) Error() string {
	return fmt.Sprintf("this test takes %d rounds, up to %d seconds; the limit is %d seconds", e.Plan.Rounds, e.Plan.Seconds, e.Plan.MaxSeconds)
}

// PlanCardTest sizes a card test over nodes given by their rails on the
// network, in test order, exactly as the run will lay it out. It returns the
// plan together with a *CardTestTooLongError when the run would exceed
// CardTestMaxSeconds.
func PlanCardTest(kind, pattern string, nodes [][]netplan.Rail) (CardTestPlan, error) {
	endpoints := make([]nettest.Endpoint, len(nodes))
	for i, rails := range nodes {
		endpoints[i].NodeUUID = strconv.Itoa(i)
		for _, rail := range rails {
			endpoints[i].Rails = append(endpoints[i].Rails, nettest.Rail{Name: rail.Name, Addr: rail.Addr})
		}
	}
	estimate := nettest.EstimateCards(endpoints, nettest.Options{Mode: kind, Pattern: pattern})
	plan := CardTestPlan{
		Nodes: len(nodes), Pairs: estimate.Pairs, Rounds: estimate.Rounds, Probes: estimate.Probes,
		Seconds: estimate.Rounds * int(nettest.CardProbeTimeout/time.Second), MaxSeconds: CardTestMaxSeconds,
	}
	if plan.Seconds > plan.MaxSeconds {
		return plan, &CardTestTooLongError{Plan: plan}
	}
	return plan, nil
}

// CardTestSummary is what a stored card run says as a whole: the shape most
// node pairs have, and how many card pairs were pinged and answered. It is
// read from the run's results each time; a run is a measurement of the moment
// it was taken, not the state of the wiring now.
type CardTestSummary struct {
	Shape     string
	Probed    int
	Reachable int
}

// SummarizeCardTest reads a card run's results.
func SummarizeCardTest(pairs []nettest.Pair) CardTestSummary {
	probed, reachable := nettest.CardCounts(pairs)
	return CardTestSummary{Shape: nettest.CardVerdict(pairs), Probed: probed, Reachable: reachable}
}

// CardPairShape reads the shape of one node pair of a card run.
func CardPairShape(pair nettest.Pair) string { return nettest.CardShape(pair) }

// DecodeTestPairs reads a stored run's results; runs stored before codes and
// paired rails decode with those fields empty. Unreadable results are none.
func DecodeTestPairs(raw string) []nettest.Pair {
	var pairs []nettest.Pair
	_ = json.Unmarshal([]byte(raw), &pairs)
	return pairs
}

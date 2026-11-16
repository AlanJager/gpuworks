package scheduler

// Card gates: conditions that withhold a card a request could otherwise use.
//
// A gate never changes what the snapshot says a card is (Schedulable,
// ForeignShareable and the rest are facts the planner reads); it marks the
// card withheld for this request (GPU.withheld), and the single admission
// predicate (GPU.takesWorkFor) refuses a withheld card. The refusal names the
// first gate that withheld a card, so the table's order is the order in which
// an operator is told what stands in the way. Future card-level conditions
// join the table.

// cardGate withholds a card a request could otherwise use.
type cardGate struct {
	// name is the gate's stable id, lower_snake.
	name string
	// applies is whether the card is subject to the gate for this request;
	// it may be any card, Schedulable or not.
	applies func(GPU, Request) bool
	// refusal explains the cards on node the gate withholds.
	refusal func(req Request, node string, withheld []GPU) Reason
}

// cardGates is ordered by precedence: the first gate that applies to a card
// withholds it.
var cardGates = []cardGate{sleepBesideForeignGate}

// sleepBesideForeignGate keeps a deployment that sleeps off a card it would
// share with processes outside the platform: its wake would be refused for
// as long as they hold the card (ADR 0068).
var sleepBesideForeignGate = cardGate{
	name: "sleep_beside_foreign",
	applies: func(g GPU, req Request) bool {
		return g.sharesForeignWith(req) && !g.Schedulable && req.sleeps()
	},
	refusal: func(Request, string, []GPU) Reason { return Reason{Code: ReasonHeldOutsidePlatformSleep} },
}

// resolveGates returns node with each card marked by the first gate that
// withholds it from req. The cards are copied; the caller's snapshot is not
// changed.
func resolveGates(req Request, node NodeSnapshot) NodeSnapshot {
	gpus := make([]GPU, len(node.GPUs))
	for i, g := range node.GPUs {
		g.withheld = 0
		for k, gate := range cardGates {
			if gate.applies(g, req) {
				g.withheld = k + 1
				break
			}
		}
		gpus[i] = g
	}
	node.GPUs = gpus
	return node
}

// gateRefusal explains the cards of gpus that the gate which withheld first
// withholds.
func gateRefusal(req Request, node string, gpus []GPU, first GPU) Reason {
	gate := cardGates[first.withheld-1]
	var withheld []GPU
	for _, g := range gpus {
		if g.withheld == first.withheld {
			withheld = append(withheld, g)
		}
	}
	return gate.refusal(req, node, withheld)
}

package nettest

// What a card run found between two nodes is read as a shape: it is the
// answer to "which cards can a deployment use together", and it is derived
// from the run's card pairs every time it is asked for, never stored. A run
// is a measurement of one moment; the wiring may have changed since.
const (
	// ShapeFull: every card pair that was pinged answered (a switched
	// fabric; in ModeCardPing, every same-named pair).
	ShapeFull = "full"
	// ShapeDiagonal: exactly the same-named cards answered: hosts cabled
	// card to card, or one plane per card.
	ShapeDiagonal = "diagonal"
	// ShapeMixed: some pairs answered, in no such pattern.
	ShapeMixed = "mixed"
	// ShapeNone: no pair answered.
	ShapeNone = "none"
	// ShapeUntested: the node pair was not probed at all (a node offline or
	// without cards, or no card had a namesake to ping).
	ShapeUntested = "untested"
)

// cardProbed lists the card pairs of a node pair that were pinged; a card
// without a namesake on the other node has a result but was never pinged.
func cardProbed(p Pair) []RailResult {
	var out []RailResult
	for _, r := range p.Rails {
		if r.Rail != "" && r.TargetRail != "" {
			out = append(out, r)
		}
	}
	return out
}

// CardShape reads the shape of one node pair of a card run.
func CardShape(p Pair) string {
	probed := cardProbed(p)
	if p.Code != "" || len(probed) == 0 {
		return ShapeUntested
	}
	ok, sameName, sameNameOK := 0, 0, 0
	for _, r := range probed {
		if r.OK() {
			ok++
		}
		if r.Rail == r.TargetRail {
			sameName++
			if r.OK() {
				sameNameOK++
			}
		}
	}
	switch {
	case ok == len(probed):
		return ShapeFull
	case ok == 0:
		return ShapeNone
	case sameName > 0 && sameNameOK == sameName && ok == sameName:
		return ShapeDiagonal
	}
	return ShapeMixed
}

// CardVerdict is the shape most node pairs of a run have; "" for a run
// without pairs. A tie goes to the healthier shape, so that a lone broken
// pair is the one that stands out.
func CardVerdict(pairs []Pair) string {
	counts := map[string]int{}
	for _, p := range pairs {
		counts[CardShape(p)]++
	}
	verdict, most := "", 0
	for _, shape := range []string{ShapeFull, ShapeDiagonal, ShapeMixed, ShapeNone, ShapeUntested} {
		if counts[shape] > most {
			verdict, most = shape, counts[shape]
		}
	}
	return verdict
}

// CardCounts is how many card pairs a run pinged and how many answered.
func CardCounts(pairs []Pair) (probed, reachable int) {
	for _, p := range pairs {
		for _, r := range cardProbed(p) {
			probed++
			if r.OK() {
				reachable++
			}
		}
	}
	return probed, reachable
}

// PairStatus is a pair's outcome in a run of the given mode. In a card mode
// it follows the shape: a pair whose cards reach each other in a usable
// pattern (all of them, or exactly the same-named ones) passed, one with no
// answer or not probed failed, anything else is partial.
func PairStatus(mode string, p Pair, bandwidth bool) string {
	if !CardMode(mode) {
		return p.Status(bandwidth)
	}
	switch CardShape(p) {
	case ShapeFull, ShapeDiagonal:
		return StatusPassed
	case ShapeMixed:
		return StatusPartial
	}
	return StatusFailed
}

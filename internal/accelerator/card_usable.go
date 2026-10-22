package accelerator

import "strings"

// Whether a card can take work, decided in one place.
//
// It used to be decided in three, each rewriting the same compatibility
// branch: an agent that predates health levels reports only a Healthy
// boolean, so "no level and not healthy" means a fault. SQL rewrote that
// branch for the fleet queries, Go rewrote it for scheduling, TypeScript
// rewrote it for the card list — and each new dimension (ownership, then
// whether the runtime can use the card) had to be added to all three,
// where missing one produces a wrong verdict in one view only.
//
// So the compatibility branch lives here and nowhere else: it turns an old
// agent's boolean into a level when the card is written (CardHealthLevel),
// and the verdict is this package's CardSchedulable asked of the card's own
// facts wherever it is needed — Go calls it per card, the fleet SQL groups
// cards by those facts and asks it per group. It is not stored: a stored
// copy could only repeat the facts or, once the rule changes, contradict
// them until every node had reported again.

// CardHealthLevel is the level to show for a card: what its probe stated,
// or — for an agent that predates levels and states only the boolean — OK
// or Error from that. An unrecognized level counts as an error, so a newer
// agent's finding is never shown as harmless.
//
// The boolean is read only when no level is stated, or an explicit OK
// comes with it false; after this, nothing reads the boolean again.
//
// ownership is read for one reason: a card this node does not own reports
// Healthy false because it cannot serve, not because anything ails it. A
// card lent to a virtual machine arrives with no level and that boolean,
// and inferring a fault from it put "故障" on four perfectly good 910B4
// (172.30.8.31, 2026-10-15) — the very "somebody looking for a fault that
// is not there" this package warns about elsewhere.
func CardHealthLevel(ownership, status string, healthy bool) string {
	switch level := NormalizeHealthLevel(status); level {
	case HealthOK:
		if healthy || !CardUsable(ownership) {
			return HealthOK
		}
		// This node's card, no level stated, and it cannot serve: the
		// boolean is all there is to go on.
		return HealthError
	default:
		return level
	}
}

// NormalizeHealthLevel reads the spellings probes have used for a level,
// and answers OK for one that states none. Everything that stores or
// judges a level puts it through here first, so "warn", "Warning" and ""
// cannot mean three different things to three readers.
func NormalizeHealthLevel(status string) string {
	// The three canonical spellings first, and by identity: every stored
	// card is at one of them, and every card of a fleet goes through here
	// on each list, overview and placement. Falling straight to ToLower
	// allocated once per card per call.
	switch status {
	case HealthOK, HealthWarning, HealthError:
		return status
	case "":
		return HealthOK
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "ok":
		return HealthOK
	case "warning", "warn":
		return HealthWarning
	default:
		return HealthError
	}
}

// CardHealthy is whether a card is in a state to serve: this node's own,
// at a health level that still serves. It is the one definition of the
// Healthy flag — the agent fills the flag from it, and the server answers
// a stored card's from the same two facts rather than keeping the agent's
// answer beside them.
//
// It takes no account of who is using the card: a card somebody outside
// the platform is running work on is perfectly healthy, and simply not
// available (CardSchedulable).
func CardHealthy(ownership, healthLevel string) bool {
	return CardUsable(ownership) && HealthSchedulable(NormalizeHealthLevel(healthLevel))
}

// CardSchedulable is whether a card can be given work: healthy, and not
// already taken by something outside the platform.
//
// It answers only "may work be placed here". Why not is the business of the
// ownership, the health level and the finding code, each of which names
// something an operator can act on; a single boolean could not.
//
// There used to be a fourth input, the Healthy boolean beside the level,
// held as a veto in case the two disagreed. They cannot: the agent fills
// that boolean from the level and the ownership (CardHealthy), and a
// report that states no level at all has one derived from the boolean
// before it is stored (CardHealthLevel). The veto only ever restated one
// of its own inputs.
func CardSchedulable(ownership, healthLevel string, heldOutsidePlatform bool) bool {
	return CardHealthy(ownership, healthLevel) && !heldOutsidePlatform
}

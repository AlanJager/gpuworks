package accelerator

import "testing"

// What is shown about a card and whether work may go on it are two
// questions, and the level answers both. A probe that states a level
// states it for a reason: a card the vendor tool calls a warning serves,
// one it calls a fault does not, and no boolean beside the level can say
// otherwise — the agent fills that boolean from the level itself
// (CardHealthy).
func TestTheLevelDecidesBothWhatIsShownAndWhetherWorkMayGo(t *testing.T) {
	level := CardHealthLevel(CardOwnedByNode, HealthWarning, false)
	if level != HealthWarning {
		t.Fatalf("shown level = %q, want the warning the tool actually found", level)
	}
	if !CardSchedulable(CardOwnedByNode, level, false) {
		t.Fatal("a card the vendor tool calls a warning still serves, after the healthy ones")
	}
	if CardSchedulable(CardOwnedByNode, CardHealthLevel(CardOwnedByNode, HealthError, false), false) {
		t.Fatal("a card the vendor tool calls a fault was scheduled")
	}
}

// The ordinary cases, where a probe's level and boolean agree.
func TestCardHealthLevelAndSchedulableAgreeOnWhatAProbeStates(t *testing.T) {
	for _, tc := range []struct {
		status      string
		healthy     bool
		wantLevel   string
		wantUsable  bool
		description string
	}{
		{HealthOK, true, HealthOK, true, "a healthy card"},
		{"", true, HealthOK, true, "an agent that predates levels, card fine"},
		{"", false, HealthError, false, "an agent that predates levels, card not fine"},
		{HealthWarning, true, HealthWarning, true, "a warning still serves, after the healthy cards"},
		{"warn", true, HealthWarning, true, "the other spelling of a warning"},
		{HealthError, false, HealthError, false, "a fault"},
		{HealthError, true, HealthError, false, "a fault, whatever the boolean says"},
		{"something new", true, HealthError, false, "an unrecognized level is never shown as harmless"},
	} {
		level := CardHealthLevel(CardOwnedByNode, tc.status, tc.healthy)
		if level != tc.wantLevel {
			t.Errorf("%s: level = %q, want %q", tc.description, level, tc.wantLevel)
		}
		if got := CardSchedulable(CardOwnedByNode, level, false); got != tc.wantUsable {
			t.Errorf("%s: schedulable = %v, want %v", tc.description, got, tc.wantUsable)
		}
	}
}

// Each reason a card cannot take work stands on its own, so none of them
// can be lost by another being absent.
func TestEachReasonAloneKeepsACardFromWork(t *testing.T) {
	if !CardSchedulable(CardOwnedByNode, HealthOK, false) {
		t.Fatal("a card with nothing against it cannot take work")
	}
	for _, tc := range []struct {
		ownership string
		level     string
		foreign   bool
		reason    string
	}{
		{CardPassedThrough, HealthOK, false, "lent to a virtual machine"},
		{CardWithoutDriver, HealthOK, false, "no driver bound"},
		{CardHeldElsewhere, HealthOK, false, "held by a driver that is not the vendor's"},
		{CardOwnedByNode, HealthError, false, "a fault"},
		{CardOwnedByNode, "something new", false, "a level this server does not recognize"},
		{CardOwnedByNode, HealthOK, true, "held by a process outside the platform"},
	} {
		if CardSchedulable(tc.ownership, tc.level, tc.foreign) {
			t.Errorf("a card %s was scheduled", tc.reason)
		}
	}
}

// A card this node does not own reports Healthy false because it cannot
// serve, not because anything ails it. Reading a fault out of that put
// "fault" on four working 910B4 handed to virtual machines (172.30.8.31,
// 2026-10-15) — and sends an operator looking for a problem that is not
// there, which is the one thing ownership must never be shown as.
func TestACardLentElsewhereIsNotShownAsFaulty(t *testing.T) {
	for _, ownership := range []string{CardPassedThrough, CardWithoutDriver} {
		if level := CardHealthLevel(ownership, "", false); level != HealthOK {
			t.Errorf("a %s card shows as %q, want no fault", ownership, level)
		}
		// It still takes no work; that is ownership's job to say, not health's.
		if CardSchedulable(ownership, HealthOK, false) {
			t.Errorf("a %s card was scheduled", ownership)
		}
	}
	// A fault the vendor tool actually found is still shown, whoever owns
	// the card: the hardware is faulty wherever it is plugged in.
	if level := CardHealthLevel(CardPassedThrough, HealthError, false); level != HealthError {
		t.Errorf("a passed-through card with a real fault = %q", level)
	}
}

// CardHealthy is the Healthy flag's one definition, and the store answers
// a card's from it rather than keeping the agent's copy beside the level.
// The two can therefore never disagree.
func TestCardHealthyIsTheLevelAndTheOwnership(t *testing.T) {
	for _, tc := range []struct {
		ownership, level string
		want             bool
		why              string
	}{
		{CardOwnedByNode, HealthOK, true, "this node's, nothing found"},
		{CardOwnedByNode, "", true, "this node's, no level stated"},
		{CardOwnedByNode, HealthWarning, true, "a warning still serves"},
		{CardOwnedByNode, HealthError, false, "a fault"},
		{CardPassedThrough, HealthOK, false, "lent to a virtual machine"},
		{CardWithoutDriver, HealthOK, false, "no driver bound"},
	} {
		if got := CardHealthy(tc.ownership, tc.level); got != tc.want {
			t.Errorf("%s: healthy = %v, want %v", tc.why, got, tc.want)
		}
	}
	// A card nothing outside the platform holds is schedulable exactly
	// when it is healthy: the two differ only in that one question.
	for _, tc := range []struct{ ownership, level string }{
		{CardOwnedByNode, HealthOK}, {CardOwnedByNode, HealthError}, {CardPassedThrough, HealthOK},
	} {
		if CardHealthy(tc.ownership, tc.level) != CardSchedulable(tc.ownership, tc.level, false) {
			t.Errorf("%s/%s: healthy and schedulable disagree with no foreign process", tc.ownership, tc.level)
		}
	}
}

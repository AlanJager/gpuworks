package dto

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SchedulingOverview is the scheduling center's view of the whole fleet in one
// response (GET /scheduling/overview): every node and, for each card, the few
// small integers a heatmap needs - no occupant detail, which a client reads
// for one node when it opens it (GET /scheduling/board?node=). It is built
// for thousands of cards, so a card is an array and a state, a model or a
// deployment is named once and referred to by its position.
type SchedulingOverview struct {
	Summary OverviewSummary `json:"summary"`
	// States are the card states (offline, unhealthy, held_outside, awake,
	// asleep, free: OverviewSummary's buckets, in their precedence) the cards
	// refer to by position. The server derives a card's state; a client only
	// names and colours it.
	States []string `json:"states"`
	// Models are the GPU model names the cards refer to by position.
	Models []string `json:"models"`
	// Deployments are the deployments that occupy a card, which the cards
	// refer to by position.
	Deployments []OverviewDeployment `json:"deployments"`
	Nodes       []OverviewNode       `json:"nodes"`
}

// OverviewSummary counts the cards of the fleet. A card is counted once, in
// the first of these that holds, in this order: Offline (its node is not
// online), Unhealthy (the vendor tool or the platform finds it faulty), HeldOutside (a process
// outside the platform uses it), Awake (an instance claims memory on it),
// Asleep (only sleepers' residue is on it), Free. They add up to Cards.
type OverviewSummary struct {
	Nodes       int                `json:"nodes"`
	OnlineNodes int                `json:"online_nodes"`
	Cards       int                `json:"cards"`
	Awake       int                `json:"awake"`
	Asleep      int                `json:"asleep"`
	Free        int                `json:"free"`
	HeldOutside int                `json:"held_outside"`
	Unhealthy   int                `json:"unhealthy"`
	Offline     int                `json:"offline"`
	Contention  OverviewContention `json:"contention"`
}

// OverviewContention is how hard the deployments push on each other for
// cards: the deployments with sleepers waiting for room to wake, the requests
// the gateway holds for a wake right now, and the deployments that gave
// their cards up for another in the last hour.
type OverviewContention struct {
	WaitingWakes   int `json:"waiting_wakes"`
	HeldRequests   int `json:"held_requests"`
	YieldsLastHour int `json:"yields_last_hour"`
}

// OverviewDeployment names a deployment that occupies a card.
type OverviewDeployment struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	// YieldPolicy is its yield policy (never, idle, timeslice, anytime).
	YieldPolicy string `json:"yield_policy"`
}

// OverviewNode is a node and its cards. Labels are the labels an operator
// wrote. Contention is the node's share of the contention (OverviewContention):
// its sleepers waiting to wake, requests held for the deployments asleep on
// it and yields of the deployments on it; absent when none.
type OverviewNode struct {
	UUID       string            `json:"uuid"`
	Name       string            `json:"name"`
	Online     bool              `json:"online"`
	Labels     map[string]string `json:"labels,omitempty"`
	Contention int               `json:"contention,omitempty"`
	Cards      []OverviewCard    `json:"cards"`
}

// OverviewCard is one card, on the wire the array
//
//	[index, model, memory_mib, awake_pct, sleeping_pct, state, switching, occupants]
//
// index is the card's device index on its node; model a position in
// SchedulingOverview.Models; awake_pct and sleeping_pct the share of the
// card's memory that awake instances claim and that sleepers keep (0-100; a
// share above 0 is never rounded down to 0); state a position in
// SchedulingOverview.States; switching 1 when an instance on the card is
// starting, going to sleep, waking or draining, else 0; and occupants, left
// out when empty, the deployments on the card as position*2 + 1 when asleep
// (position in SchedulingOverview.Deployments).
type OverviewCard struct {
	Index       int
	Model       int
	MemoryMiB   int64
	AwakePct    int
	SleepingPct int
	State       int
	Switching   bool
	Occupants   []int
}

// MarshalJSON writes the card as its array.
func (c OverviewCard) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	switching := 0
	if c.Switching {
		switching = 1
	}
	fmt.Fprintf(&b, "[%d,%d,%d,%d,%d,%d,%d", c.Index, c.Model, c.MemoryMiB, c.AwakePct, c.SleepingPct, c.State, switching)
	if len(c.Occupants) > 0 {
		b.WriteString(",[")
		for i, occupant := range c.Occupants {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", occupant)
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// UnmarshalJSON reads the card from its array.
func (c *OverviewCard) UnmarshalJSON(data []byte) error {
	var fields []json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 7 && len(fields) != 8 {
		return fmt.Errorf("a card has 7 or 8 elements, not %d", len(fields))
	}
	var numbers [7]int64
	for i := range numbers {
		if err := json.Unmarshal(fields[i], &numbers[i]); err != nil {
			return fmt.Errorf("card element %d: %w", i, err)
		}
	}
	if numbers[6] != 0 && numbers[6] != 1 {
		return fmt.Errorf("card switching is 0 or 1, not %d", numbers[6])
	}
	*c = OverviewCard{Index: int(numbers[0]), Model: int(numbers[1]), MemoryMiB: numbers[2], AwakePct: int(numbers[3]), SleepingPct: int(numbers[4]), State: int(numbers[5]), Switching: numbers[6] == 1}
	if len(fields) == 8 {
		if err := json.Unmarshal(fields[7], &c.Occupants); err != nil {
			return fmt.Errorf("card occupants: %w", err)
		}
	}
	return nil
}

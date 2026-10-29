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
// for thousands of cards, so a card is an array and a model or deployment is
// named once and referred to by its position.
type SchedulingOverview struct {
	Summary OverviewSummary `json:"summary"`
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
	Nodes       int                    `json:"nodes"`
	OnlineNodes int                    `json:"online_nodes"`
	Cards       int                    `json:"cards"`
	Awake       int                    `json:"awake"`
	Asleep      int                    `json:"asleep"`
	Free        int                    `json:"free"`
	HeldOutside int                    `json:"held_outside"`
	Unhealthy   int                    `json:"unhealthy"`
	Offline     int                    `json:"offline"`
	ByModel     []OverviewModelSummary `json:"by_model"`
	Contention  OverviewContention     `json:"contention"`
}

// OverviewModelSummary is the same count for the cards of one GPU model.
type OverviewModelSummary struct {
	Model       string `json:"model"`
	Cards       int    `json:"cards"`
	Awake       int    `json:"awake"`
	Asleep      int    `json:"asleep"`
	Free        int    `json:"free"`
	HeldOutside int    `json:"held_outside"`
	Unhealthy   int    `json:"unhealthy"`
	Offline     int    `json:"offline"`
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
	// Preset is the service level its yield policy stands for, or custom.
	Preset string `json:"preset"`
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

// The bits of OverviewCard.Flags.
const (
	OverviewHeldOutside = 1 // a process outside the platform uses the card
	OverviewSwitching   = 2 // an instance on it is starting, going to sleep, waking or draining
	OverviewUnhealthy   = 4 // the card is faulty or unusable for a reason other than being held
)

// OverviewCard is one card, on the wire the array
//
//	[index, model, memory_mib, awake_pct, sleeping_pct, flags, occupants]
//
// index is the card's device index on its node; model a position in
// SchedulingOverview.Models; awake_pct and sleeping_pct the share of the
// card's memory that awake instances claim and that sleepers keep (0-100; a
// share above 0 is never rounded down to 0); flags the Overview* bits; and
// occupants, left out when empty, the deployments on the card as
// position*2 + 1 when asleep (position in SchedulingOverview.Deployments).
type OverviewCard struct {
	Index       int
	Model       int
	MemoryMiB   int64
	AwakePct    int
	SleepingPct int
	Flags       int
	Occupants   []int
}

// MarshalJSON writes the card as its array.
func (c OverviewCard) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "[%d,%d,%d,%d,%d,%d", c.Index, c.Model, c.MemoryMiB, c.AwakePct, c.SleepingPct, c.Flags)
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
	if len(fields) != 6 && len(fields) != 7 {
		return fmt.Errorf("a card has 6 or 7 elements, not %d", len(fields))
	}
	var numbers [6]int64
	for i := range numbers {
		if err := json.Unmarshal(fields[i], &numbers[i]); err != nil {
			return fmt.Errorf("card element %d: %w", i, err)
		}
	}
	*c = OverviewCard{Index: int(numbers[0]), Model: int(numbers[1]), MemoryMiB: numbers[2], AwakePct: int(numbers[3]), SleepingPct: int(numbers[4]), Flags: int(numbers[5])}
	if len(fields) == 7 {
		if err := json.Unmarshal(fields[6], &c.Occupants); err != nil {
			return fmt.Errorf("card occupants: %w", err)
		}
	}
	return nil
}

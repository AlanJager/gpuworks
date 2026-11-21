package scheduler_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

// This is a bounded scheduler input model, not a universal test runner.
// Add a dimension here, in input and in expectation; every combination of the
// complete valid domain is then checked, without editing existing case rows.
const (
	modeDimension = iota
	occupancyDimension
	residueDimension
	foreignDimension
	backendDimension
	healthDimension
	selectionDimension
	cardDecisionDimension
	sleepDimension
	dimensionCount
)

var scheduleDimensions = [dimensionCount][]string{
	modeDimension:         {"whole", "partition", "native"},
	occupancyDimension:    {"empty", "half", "three-quarters"},
	residueDimension:      {"no-residue", "residue"},
	foreignDimension:      {"no-foreign", "known-foreign", "unknown-foreign"},
	backendDimension:      {"backend-ready", "backend-absent"},
	healthDimension:       {"healthy", "stopped"},
	selectionDimension:    {"automatic", "operator-pin", "platform-pin"},
	cardDecisionDimension: {"not-enabled", "enabled"},
	sleepDimension:        {"awake-only", "sleepable"},
}

type scheduleCombination [dimensionCount]string

func (c scheduleCombination) name() string { return strings.Join(c[:], "/") }

func (c scheduleCombination) input() scheduleInput {
	g := card(7)
	g.AllocatedBytes = map[string]int64{"empty": 0, "half": 32 * gib, "three-quarters": 48 * gib}[c[occupancyDimension]]
	if c[residueDimension] == "residue" {
		g.SleepingBytes = 8 * gib
	}
	if c[backendDimension] == "backend-absent" {
		g.PartitionBackends = nil
	}
	// An operator who stopped the card has no enable decision on it
	// (store.SetCardScheduling keeps one or the other); one who enabled it
	// lets work on whatever holds it.
	g.Schedulable = c[healthDimension] == "healthy" && (c[foreignDimension] == "no-foreign" || c.enabled())
	g.HeldOutsidePlatform = c[foreignDimension] != "no-foreign"
	g.ForeignShareable = c[foreignDimension] == "known-foreign" && c[healthDimension] == "healthy"
	if c[foreignDimension] == "known-foreign" {
		g.ForeignBytes = fixtures.ForeignHolderBytes
	}
	r := scheduler.Request{WeightBytes: 8 * gib, ReserveBytes: 2 * gib, GPUMemUtil: 0.5, AllowedTPs: []int{1}}
	switch c[modeDimension] {
	case "partition":
		r.PerGPULimitBytes = 16 * gib
		r.PartitionBackend = accelerator.BackendNVIDIALogical
	case "native":
		g.SharedByMemory = true
		r.UtilizationIsShare = true
	}
	if c[selectionDimension] != "automatic" {
		r.AllowedGPUIndexes = []int{7}
	}
	r.Consent.CardsChosen = c[selectionDimension] == "operator-pin"
	if c[sleepDimension] == "sleepable" {
		r.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	}
	return scheduleInput{request: r, node: scheduler.NodeSnapshot{UUID: "combinations", Vendor: "nvidia", DtypeSupported: true, GPUs: []scheduler.GPU{g}}}
}

// enabled is whether an operator enabled scheduling on the card.
func (c scheduleCombination) enabled() bool { return c[cardDecisionDimension] == "enabled" }

// The oracle uses this fixture's explicit budgets, not GPU.UsableFor,
// CapacityFor, Claim, or another production admission helper. For a 64 GiB
// card: partition = 16 + 0.5 GiB; native engine share = 32 GiB; external
// holder + ADR 0068 headroom = 16 + 4 GiB; whole startup = 32 GiB.
func (c scheduleCombination) expectation() scheduleExpectation {
	room := map[string]int64{"empty": 64 * gib, "half": 32 * gib, "three-quarters": 16 * gib}[c[occupancyDimension]]
	if c[residueDimension] == "residue" {
		room -= 8 * gib
	}
	ok := c[healthDimension] == "healthy"
	if c[foreignDimension] != "no-foreign" {
		// A card the operator enabled takes every kind of work; otherwise
		// only a partition the operator pinned, beside holders of known
		// memory, and never one that sleeps.
		if !c.enabled() {
			ok = ok && c[modeDimension] == "partition" && c[foreignDimension] == "known-foreign" && c[sleepDimension] == "awake-only" && c[selectionDimension] == "operator-pin"
		}
		if c[foreignDimension] == "known-foreign" {
			room -= fixtures.ForeignHolderBytes + fixtures.ForeignHeadroom
		}
	}
	switch c[modeDimension] {
	case "whole":
		ok = ok && c[occupancyDimension] == "empty" && room >= 32*gib
	case "partition":
		ok = ok && c[backendDimension] == "backend-ready" && room >= fixtures.NVIDIASliceClaim(16*gib)
	case "native":
		ok = ok && room >= 32*gib
	default:
		panic("unmodelled allocation mode")
	}
	return scheduleExpectation{ok: ok, indexes: []int{7}, tp: 1}
}

func scheduleCombinationInputs() []scheduleCombination {
	rows := []scheduleCombination{{}}
	for dimension, values := range scheduleDimensions {
		var next []scheduleCombination
		for _, row := range rows {
			for _, value := range values {
				changed := row
				changed[dimension] = value
				next = append(next, changed)
			}
		}
		rows = next
	}
	return rows
}

func (c scheduleCombination) eligibilityGatesOpen() bool {
	return c[foreignDimension] == "no-foreign" && c[backendDimension] == "backend-ready" && c[healthDimension] == "healthy"
}

// Schedule is a pure function, so the full product is cheaper to check than to
// sample. Each mode still needs an accepted and a refused row with every
// eligibility gate open: otherwise an unrelated refusal could be the only
// negative evidence for that mode.
func TestScheduleCombinations(t *testing.T) {
	accepted, refused := map[string]int{}, map[string]int{}
	for _, row := range scheduleCombinationInputs() {
		want := row.expectation()
		if row.eligibilityGatesOpen() {
			if want.ok {
				accepted[row[modeDimension]]++
			} else {
				refused[row[modeDimension]]++
			}
		}
		t.Run(row.name(), func(t *testing.T) {
			in := row.input()
			before := slices.Clone(in.node.GPUs)
			for i := range before {
				before[i].PartitionBackends = slices.Clone(before[i].PartitionBackends)
			}
			runScheduleCase(t, scheduleCase{input: in, want: want})
			if !reflect.DeepEqual(before, in.node.GPUs) {
				t.Fatal("scheduling mutated the inventory")
			}
		})
	}
	for _, mode := range scheduleDimensions[modeDimension] {
		if accepted[mode] == 0 || refused[mode] == 0 {
			t.Errorf("%s has no accepted or refused row with every gate open", mode)
		}
	}
}

// Hold memory and capabilities in a fitting state so the card's decision, consent and sleep
// cannot be masked by a shortage. In particular a platform pin is not consent.
func TestForeignConsentAndSleepCombinations(t *testing.T) {
	for _, selection := range scheduleDimensions[selectionDimension] {
		for _, decision := range scheduleDimensions[cardDecisionDimension] {
			for _, sleep := range scheduleDimensions[sleepDimension] {
				row := scheduleCombination{modeDimension: "partition", occupancyDimension: "empty", residueDimension: "no-residue", foreignDimension: "known-foreign", backendDimension: "backend-ready", healthDimension: "healthy", selectionDimension: selection, cardDecisionDimension: decision, sleepDimension: sleep}
				want := row.expectation()
				if !want.ok {
					want.code = scheduler.ReasonHeldOutsidePlatformNotAllowed
					if sleep == "sleepable" {
						want.code = scheduler.ReasonHeldOutsidePlatformSleep
					}
				}
				t.Run(row.name(), func(t *testing.T) { runScheduleCase(t, scheduleCase{input: row.input(), want: want}) })
			}
		}
	}
}

package scheduler_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

// This is a bounded scheduler input model, not a universal test runner.
// Add a dimension here, in input and in expectation; coverage is then rebuilt
// from the complete valid domain, without editing existing case rows.
const (
	modeDimension = iota
	occupancyDimension
	residueDimension
	foreignDimension
	backendDimension
	healthDimension
	selectionDimension
	policyDimension
	sleepDimension
	dimensionCount
)

var scheduleDimensions = [dimensionCount][]string{
	modeDimension:      {"whole", "partition", "native"},
	occupancyDimension: {"empty", "half", "three-quarters"},
	residueDimension:   {"no-residue", "residue"},
	foreignDimension:   {"no-foreign", "known-foreign", "unknown-foreign"},
	backendDimension:   {"backend-ready", "backend-absent"},
	healthDimension:    {"healthy", "stopped"},
	selectionDimension: {"automatic", "operator-pin", "platform-pin"},
	policyDimension:    {"sharing-off", "sharing-on"},
	sleepDimension:     {"awake-only", "sleepable"},
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
	g.Schedulable = c[healthDimension] == "healthy" && c[foreignDimension] == "no-foreign"
	g.HeldOutsidePlatform = c[foreignDimension] != "no-foreign"
	g.ForeignShareable = c[foreignDimension] == "known-foreign" && c[healthDimension] == "healthy"
	if c[foreignDimension] == "known-foreign" {
		g.ForeignBytes = 16 * gib
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
	return scheduleInput{request: r, node: scheduler.NodeSnapshot{UUID: "combinations", Vendor: "nvidia", DtypeSupported: true,
		Policy: nodepolicy.Policy{ShareWithForeign: c[policyDimension] == "sharing-on"}, GPUs: []scheduler.GPU{g}}}
}

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
		ok = ok && c[modeDimension] == "partition" && c[foreignDimension] == "known-foreign" && c[sleepDimension] == "awake-only" && (c[selectionDimension] == "operator-pin" || c[policyDimension] == "sharing-on")
		room -= 20 * gib
	}
	switch c[modeDimension] {
	case "whole":
		ok = ok && c[occupancyDimension] == "empty" && room >= 32*gib
	case "partition":
		ok = ok && c[backendDimension] == "backend-ready" && room >= 16*gib+gib/2
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

type dimensionPair struct {
	first, second int
	a, b          string
}

func pairs(c scheduleCombination) []dimensionPair {
	var out []dimensionPair
	for i := range c {
		for j := i + 1; j < len(c); j++ {
			out = append(out, dimensionPair{i, j, c[i], c[j]})
		}
	}
	return out
}

// Greedy selection is deterministic (domain order breaks ties). The universe
// is deliberately small; larger models should use a checked-in covering
// array from a constraint-aware tool, with the same coverage assertion.
func pairwiseScheduleInputs(domain []scheduleCombination) []scheduleCombination {
	missing := map[dimensionPair]bool{}
	for _, row := range domain {
		for _, pair := range pairs(row) {
			missing[pair] = true
		}
	}
	var selected []scheduleCombination
	for len(missing) > 0 {
		best, score := -1, 0
		for i, row := range domain {
			count := 0
			for _, pair := range pairs(row) {
				if missing[pair] {
					count++
				}
			}
			if count > score {
				best, score = i, count
			}
		}
		if best < 0 {
			panic("unreachable pair")
		}
		selected = append(selected, domain[best])
		for _, pair := range pairs(domain[best]) {
			delete(missing, pair)
		}
	}
	return selected
}

func assertScheduleCombinationCoverage(t *testing.T, rows []scheduleCombination) {
	t.Helper()
	seen := map[dimensionPair]bool{}
	for _, row := range rows {
		for i, value := range row {
			if !slices.Contains(scheduleDimensions[i], value) {
				t.Fatalf("unknown dimension %d=%s", i, value)
			}
		}
		for _, pair := range pairs(row) {
			seen[pair] = true
		}
	}
	// Enumerate expected pairs independently of the row generator.
	for i, values := range scheduleDimensions {
		for j := i + 1; j < len(scheduleDimensions); j++ {
			for _, a := range values {
				for _, b := range scheduleDimensions[j] {
					if !seen[dimensionPair{i, j, a, b}] {
						t.Errorf("missing pair: dimension %d=%s / %d=%s", i, a, j, b)
					}
				}
			}
		}
	}
}

func TestScheduleCombinations(t *testing.T) {
	domain := scheduleCombinationInputs()
	rows := pairwiseScheduleInputs(domain)
	assertScheduleCombinationCoverage(t, rows)
	t.Logf("%d pairwise rows from %d combinations", len(rows), len(domain))
	for _, row := range rows {
		t.Run(row.name(), func(t *testing.T) {
			in := row.input()
			before := append([]scheduler.GPU(nil), in.node.GPUs...)
			runScheduleCase(t, scheduleCase{input: in, want: row.expectation()})
			if !reflect.DeepEqual(before, in.node.GPUs) {
				t.Fatal("scheduling mutated the inventory")
			}
		})
	}
}

// Pairwise coverage alone can hide a bug behind an unrelated refusal. Fix all
// eligibility gates open and exhaust mode × occupancy × residue × selection.
// Both successful and rejected placements are required for each mode.
func TestCapacityAndSelectionCombinations(t *testing.T) {
	for _, mode := range scheduleDimensions[modeDimension] {
		accepted, refused := 0, 0
		for _, occupied := range scheduleDimensions[occupancyDimension] {
			for _, residue := range scheduleDimensions[residueDimension] {
				for _, selection := range scheduleDimensions[selectionDimension] {
					row := scheduleCombination{modeDimension: mode, occupancyDimension: occupied, residueDimension: residue, foreignDimension: "no-foreign", backendDimension: "backend-ready", healthDimension: "healthy", selectionDimension: selection, policyDimension: "sharing-off", sleepDimension: "awake-only"}
					want := row.expectation()
					if want.ok {
						accepted++
					} else {
						refused++
					}
					t.Run(row.name(), func(t *testing.T) { runScheduleCase(t, scheduleCase{input: row.input(), want: want}) })
				}
			}
		}
		if accepted == 0 || refused == 0 {
			t.Fatalf("%s has no positive or negative witness", mode)
		}
	}
}

// Hold memory and capabilities in a fitting state so policy, consent and sleep
// cannot be masked by a shortage. In particular a platform pin is not consent.
func TestForeignConsentAndSleepCombinations(t *testing.T) {
	for _, selection := range scheduleDimensions[selectionDimension] {
		for _, policy := range scheduleDimensions[policyDimension] {
			for _, sleep := range scheduleDimensions[sleepDimension] {
				row := scheduleCombination{modeDimension: "partition", occupancyDimension: "empty", residueDimension: "no-residue", foreignDimension: "known-foreign", backendDimension: "backend-ready", healthDimension: "healthy", selectionDimension: selection, policyDimension: policy, sleepDimension: sleep}
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

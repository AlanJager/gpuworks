package planning

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An operator's decision about a card reaches the scheduler through the
// snapshot (the card takes work), and their choice of cards for one
// deployment through Input.Consent.
func TestEvaluateAppliesTheCardsDecisionAndTheOperatorsConsent(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	node.GPUs[1].ForeignProcesses = node.GPUs[0].ForeignProcesses // both cards held
	in := Input{
		Model: &store.Model{WeightBytes: 15 << 30}, Node: node, Engine: "vllm",
		Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, VRAMLimitBytes: 50 << 30,
		PartitionBackend: accelerator.BackendNVIDIALogical,
	}
	out := in.Evaluate(st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonHeldOutsidePlatformNotAllowed ||
		out.Result.Message.Params["node"] != node.UUID || out.Result.Message.Params["count"] != 2 {
		t.Fatalf("not enabled, automatic = ok %v %+v", out.Result.OK, out.Result.Message)
	}
	if _, named := out.Result.Message.Params["setting"]; named {
		t.Fatalf("the refusal names a setting: %+v", out.Result.Message.Params)
	}
	// The card list is judged as picked by the operator.
	if !candidateAvailable(t, out.GPUCandidates, 0) {
		t.Fatalf("held card not offered while not enabled: %+v", out.GPUCandidates)
	}
	for _, candidate := range out.GPUCandidates {
		if candidate.Index == 0 && (candidate.Note == nil || candidate.Note.Code != scheduler.ReasonForeignSharedCard) {
			t.Fatalf("held card note = %+v", candidate.Note)
		}
	}
	chosen := in
	chosen.GPUIndexes, chosen.Consent = []int{0}, scheduler.Consent{CardsChosen: true}
	if out := chosen.Evaluate(st); !out.Result.OK || out.Result.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("not enabled, chosen card = ok %v %+v", out.Result.OK, out.Result.Message)
	}
	// Enabling scheduling on card 1 lets the scheduler choose it, and the
	// plan says it shares the card.
	now := time.Now().UTC()
	node.GPUs[1].SchedulingAllowedAt = &now
	out = in.Evaluate(st)
	if !out.Result.OK || out.Result.Plan == nil || out.Result.Plan.GPUIndexes[0] != 1 {
		t.Fatalf("card 1 enabled = ok %v %+v plan %+v, want GPU 1", out.Result.OK, out.Result.Message, out.Result.Plan)
	}
	var noted bool
	for _, reason := range out.Result.Plan.Reasons {
		noted = noted || reason.Code == scheduler.ReasonForeignSharedGPUsUsed
	}
	if !noted {
		t.Fatalf("plan reasons = %+v, want %s", out.Result.Plan.Reasons, scheduler.ReasonForeignSharedGPUsUsed)
	}
}

// Consent comes from the "gpus" key alone: a launch built from a stored
// placement carries it, an echoed plan ("gpu_indexes") and an unpinned
// placement do not.
func TestLaunchInputConsentsOnlyForTheOperatorsCards(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	model := &store.Model{WeightBytes: 15 << 30}
	for _, tc := range []struct {
		name      string
		placement string
		want      bool
	}{
		{"operator's cards", `{"gpus":[0]}`, true},
		{"echoed plan", `{"gpu_indexes":[0]}`, false},
		{"scheduler's choice", `{"tp":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placement, err := ParsePlacement(tc.placement)
			if err != nil {
				t.Fatal(err)
			}
			input, err := launchInput(st, LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: placement, Topology: DistTopology{NNodes: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if input.Consent.CardsChosen != tc.want {
				t.Fatalf("consent = %+v, want CardsChosen %v", input.Consent, tc.want)
			}
		})
	}
}

// A replica the pool places elsewhere lets the scheduler pick its cards:
// DeploymentLaunch drops the indexes, and the consent with them.
func TestDeploymentLaunchOffItsPinnedNodeCarriesNoConsent(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	dep := &store.Deployment{Engine: "vllm", Placement: `{"gpus":[0]}`, NNodes: 1}
	pinned := map[string][]string{"gpuworks.ai/node": {node.UUID}}
	for _, tc := range []struct {
		name string
		reqs map[string][]string
		want bool
	}{
		{"pinned to the node", pinned, true},
		{"placed by the pool", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, _, err := DeploymentLaunch(dep, tc.reqs, node, &store.Model{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.Placement.OperatorPinned() != tc.want {
				t.Fatalf("operator pinned = %v, want %v (%+v)", req.Placement.OperatorPinned(), tc.want, req.Placement)
			}
		})
	}
}

// What the holders take is kept out of what the platform may offer whether
// or not an operator enabled scheduling on the card.
func TestForeignHoldIsCountedWhateverTheCardsDecision(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	if node.GPUs[0].CanTakeWork() {
		t.Fatal("a held card nobody enabled takes work")
	}
	if bytes, known := ForeignHold(node, &node.GPUs[0]); !known || bytes <= reporterForeign.MemoryBytes {
		t.Fatalf("foreign hold = %d %v, want the holder's memory and headroom", bytes, known)
	}
	now := time.Now().UTC()
	node.GPUs[0].SchedulingAllowedAt = &now
	if !node.GPUs[0].CanTakeWork() {
		t.Fatal("an enabled held card takes no work")
	}
	if bytes, known := ForeignHold(node, &node.GPUs[0]); !known || bytes <= reporterForeign.MemoryBytes {
		t.Fatalf("foreign hold on an enabled card = %d %v", bytes, known)
	}
}

// Cards an operator pins on a PD unit or a multi-node rank are the
// operator's choice as much as the top-level "gpus": the rank's launch
// carries the consent, so a hand-picked held card that fits by memory is
// admitted though nobody enabled scheduling on the card. A unit the platform places names
// no cards, and gives none.
func TestPDRankLaunchConsentsForTheUnitsPinnedCards(t *testing.T) {
	st := foreignStore(t)
	node := foreignHeldNode(t, st, "h20", proto.PartitionSafetyForeignTracked, reporterForeign)
	model := &store.Model{WeightBytes: 15 << 30}
	mode, _ := pdmode.Lookup("vllm-nixl")
	pd := &PDPlacement{}
	for _, tc := range []struct {
		name string
		unit PDUnit
		want bool
	}{
		{"pinned unit", PDUnit{NodeUUID: node.UUID, GPUIndexes: []int{0}}, true},
		{"placed unit", PDUnit{NodeUUID: node.UUID}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := LaunchRequest{Engine: "vllm", Model: model, Node: node}
			pd.RankLaunch(mode, pdmode.RolePrefill, tc.unit, 0, DistTopology{NNodes: 1}, PDRankPorts{}).Apply(&req)
			input, err := launchInput(st, req)
			if err != nil {
				t.Fatal(err)
			}
			if input.Consent.CardsChosen != tc.want {
				t.Fatalf("consent = %+v, want CardsChosen %v", input.Consent, tc.want)
			}
		})
	}
}

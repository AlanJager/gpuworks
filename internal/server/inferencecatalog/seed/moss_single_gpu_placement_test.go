package seed

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The MOSS delay-pattern deploy configs give stage 0 one device, so a launch
// with --tensor-parallel-size 2 exits before the engine is up. Automatic
// placement must therefore answer a card that is too small with a larger card
// or a refusal, never with a second card.
func TestMossDelaySpecsAreScheduledOnOneGPU(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "moss.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const big, small = "openmoss-team/moss-tts", "openmoss-team/moss-voicegenerator"
	cases := []struct {
		name     string
		model    string
		cardsGiB []int64
		selected []int // operator-pinned cards; nil is automatic placement
		wantGPUs []int // nil means the node is refused
	}{
		// 22.4 GiB of weights and codec at 0.75 need about 31 GiB on one card.
		{"two 24 GiB cards are refused, not paired", big, []int64{24, 24}, nil, nil},
		{"eight 24 GiB cards are refused", big, []int64{24, 24, 24, 24, 24, 24, 24, 24}, nil, nil},
		{"one 48 GiB card", big, []int64{48}, nil, []int{0}},
		{"four 48 GiB cards use one", big, []int64{48, 48, 48, 48}, nil, []int{0}},
		{"the larger card of a mixed node", big, []int64{24, 24, 48}, nil, []int{2}},
		{"the larger card behind seven small ones", big, []int64{24, 24, 24, 24, 24, 24, 24, 141}, nil, []int{7}},
		{"one H20-3e", big, []int64{141}, nil, []int{0}},
		{"two pinned 48 GiB cards are refused", big, []int64{48, 48}, []int{0, 1}, nil},
		{"one pinned 48 GiB card", big, []int64{48, 48}, []int{1}, []int{1}},
		// 10.6 GiB of weights and codec at 0.75 fit a 24 GiB card.
		{"the small model on a 24 GiB card", small, []int64{24, 24}, nil, []int{0}},
		{"the small model on a 16 GiB pair is refused", small, []int64{16, 16}, nil, nil},
	}
	for _, modelID := range []string{big, "openmoss-team/moss-soundeffect", small} {
		for _, tc := range cases {
			// MOSS-SoundEffect is sized like MOSS-TTS.
			if tc.model != modelID && !(tc.model == big && modelID == "openmoss-team/moss-soundeffect") {
				continue
			}
			t.Run(modelID+"/"+tc.name, func(t *testing.T) {
				out := mossEvaluate(t, st, modelID, tc.cardsGiB, tc.selected, parallelism.Spec{})
				if tc.wantGPUs == nil {
					if out.Result.OK {
						t.Fatalf("admitted on %v GiB with plan %+v, want a refusal", tc.cardsGiB, out.Result.Plan)
					}
					for _, rejected := range out.Result.Explain.Rejected {
						if rejected.TP > 1 && rejected.Reason.Code != scheduler.ReasonPlacementRejected {
							t.Errorf("tp=%d was refused for %s, want the single-GPU contract to refuse it", rejected.TP, rejected.Reason.Code)
						}
					}
					return
				}
				if !out.Result.OK || out.Result.Plan == nil {
					t.Fatalf("refused on %v GiB: %+v; placement error=%v", tc.cardsGiB, out.Result, out.PlacementError)
				}
				if out.PlacementError != nil {
					t.Fatalf("placement error: %v", out.PlacementError)
				}
				if out.Result.Plan.TP != 1 || !slices.Equal(out.Result.Plan.GPUIndexes, tc.wantGPUs) {
					t.Fatalf("plan tp=%d gpus=%v, want tp=1 gpus=%v", out.Result.Plan.TP, out.Result.Plan.GPUIndexes, tc.wantGPUs)
				}
			})
		}
	}
}

// A multi-node instance is planned rank by rank, each rank on its own node's
// cards, so two nodes with a card apiece show the resolver one card twice. The
// launch is still --tensor-parallel-size 2 (or two pipeline stages) over two
// devices, which stage 0 cannot run: every rank must be refused by the
// single-GPU contract, on cards large enough that memory is not the reason.
func TestMossDelaySpecsAreRefusedAcrossNodes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "moss.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cases := []struct {
		name     string
		parallel parallelism.Spec
	}{
		{"tensor parallel 2 over two nodes", parallelism.Spec{TP: 2, NNodes: 2}},
		{"pipeline parallel 2 over two nodes", parallelism.Spec{TP: 1, PP: 2, NNodes: 2}},
		{"data parallel 2 over two nodes", parallelism.Spec{TP: 1, DP: 2, NNodes: 2}},
	}
	for _, modelID := range []string{"openmoss-team/moss-tts", "openmoss-team/moss-soundeffect", "openmoss-team/moss-voicegenerator"} {
		for _, tc := range cases {
			// 24 GiB is the card the pairing was reported on; one H20-3e runs
			// any of the three models alone.
			for _, cardGiB := range []int64{24, 141} {
				t.Run(fmt.Sprintf("%s/%s/%d GiB", modelID, tc.name, cardGiB), func(t *testing.T) {
					// The rank's node has one card, as each node of the group does.
					out := mossEvaluate(t, st, modelID, []int64{cardGiB}, nil, tc.parallel)
					if out.Result.OK {
						t.Fatalf("a rank of %+v was admitted with plan %+v, want a refusal", tc.parallel, out.Result.Plan)
					}
					if out.Result.Message.Code != scheduler.ReasonPlacementRejected {
						t.Fatalf("refused for %+v, want the single-GPU contract to refuse it", out.Result.Message)
					}
				})
			}
		}
		t.Run(modelID+"/one H20-3e on one node", func(t *testing.T) {
			out := mossEvaluate(t, st, modelID, []int64{141}, nil, parallelism.Spec{TP: 1, NNodes: 1})
			if !out.Result.OK || out.PlacementError != nil {
				t.Fatalf("refused: %+v; placement error=%v", out.Result.Message, out.PlacementError)
			}
		})
	}
}

func mossEvaluate(t *testing.T, st *store.Store, modelID string, cardsGiB []int64, selected []int, parallel parallelism.Spec) planning.Output {
	t.Helper()
	model, spec := catalogSpec(t, modelID, "bf16-vllm-nvidia-general")
	meta, err := json.Marshal(model.ModelConfig)
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{UUID: "moss", GPUVendor: "nvidia"}
	cards := make([]deploy.CardFacts, len(cardsGiB))
	for index, gib := range cardsGiB {
		node.GPUs = append(node.GPUs, store.GPU{Index: index, Name: "NVIDIA test card", Vendor: "nvidia",
			ComputeCapability: "9.0", MemoryTotal: gib << 30})
		cards[index] = deploy.CardFacts{Name: "NVIDIA test card", MemoryTotalBytes: gib << 30}
	}
	paths, err := json.Marshal([]proto.PathUsage{{Kind: "models", AvailableBytes: 2 << 40}})
	if err != nil {
		t.Fatal(err)
	}
	node.PathUsage = string(paths)
	materialized, err := deploy.Materialize(deploy.MaterializeInput{Engine: spec.Engine, TemplateArgs: spec.Args, Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	var componentBytes int64
	for _, component := range spec.Components {
		if component.Role != "primary" {
			componentBytes += component.WeightBytes
		}
	}
	resolver := ""
	if spec.Placement != nil {
		resolver = spec.Placement.Resolver
	}
	return (planning.Input{
		Model: &store.Model{Name: model.ModelID, Meta: string(meta), WeightBytes: spec.WeightBytes},
		Node:  node, Engine: spec.Engine, Quick: materialized.Quick, GPUIndexes: selected,
		ComponentWeightBytes: componentBytes, PlacementResolver: resolver, Parallel: parallel,
	}).Evaluate(st)
}

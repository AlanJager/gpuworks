package deployment_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

func service(t *testing.T, w fixtures.World, afterPick func(string)) *deployment.Service {
	t.Helper()
	return deployment.New(deployment.Deps{Store: w.Store, Online: func(string) bool { return true }, AfterAutomaticPick: afterPick,
		ImageExists: func(context.Context, string, string) (bool, error) { return true, nil },
		FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
			t.Error("allocation with a complete local model attempted a remote metadata read")
			return nil, errors.New("remote metadata disabled in allocation tests")
		}})
}

func request(w fixtures.World) dto.DeployRequest {
	return dto.DeployRequest{Name: "newcomer", ServedName: "newcomer", ModelID: w.Model.ID, Engine: "vllm", Image: "example/vllm:test", TargetVendor: "nvidia",
		PlacementMode: "auto", Placement: map[string]any{"tp": 1}, QuickParams: map[string]any{"max_model_len": 256, "gpu_memory_utilization": 0.9}}
}

// Exercise the public Evaluate/Create boundary. Manual creation records intent
// while cards are busy; automatic creation must find room now. An echoed
// automatic plan must not accidentally grant the consent of a manual pin.
func TestPlacementModeByAllocationAndOccupancy(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "auto-echo"} {
		for _, partition := range []bool{false, true} {
			for _, occupancy := range []string{"free", "whole", "partition", "foreign"} {
				for _, allowForeign := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/partition-%v/%s/sharing-%v", mode, partition, occupancy, allowForeign), func(t *testing.T) {
						w := fixtures.NewWorld(t)
						if err := w.Store.DB().Model(w.Node).Update("placement_share_with_foreign", allowForeign).Error; err != nil {
							t.Fatal(err)
						}
						baseRows := 0
						if occupancy == "whole" || occupancy == "partition" {
							limit := int64(0)
							if occupancy == "partition" {
								limit = 16 * fixtures.GiB
							}
							holder := w.Deployment(t, "holder", limit)
							baseRows = 1
							for index := range w.Node.GPUs {
								w.Instance(t, holder, fmt.Sprintf("holder-%d", index), proto.InstRunning, "", index, 0)
							}
						}
						if occupancy == "foreign" {
							for i := range w.Node.GPUs {
								if err := w.Store.DB().Model(&w.Node.GPUs[i]).Update("foreign_processes", `[{"pid":42,"name":"outside","memory_bytes":17179869184}]`).Error; err != nil {
									t.Fatal(err)
								}
							}
						}
						req := request(w)
						if partition {
							req.VRAMLimitGiB = 16
						}
						if mode == "manual" {
							req.PlacementMode = "manual"
							req.NodeUUID = w.Node.UUID
						}
						if mode != "auto" {
							req.Placement["gpus"] = []int{0}
						}
						wantFits := occupancy == "free" || (occupancy == "partition" && partition) || (occupancy == "foreign" && partition && (allowForeign || mode == "manual"))
						svc := service(t, w, nil)
						preview, err := svc.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: req})
						// Automatic OK validates the draft; current room is expressed by
						// SelectedNode and Candidates. Manual OK includes its node plan.
						wantPreviewOK := wantFits || mode != "manual"
						if err != nil || len(preview.Errors) != 0 || preview.OK != wantPreviewOK {
							t.Fatalf("preview OK=%v errors=%+v err=%v, want fit=%v plan=%+v candidates=%+v", preview.OK, preview.Errors, err, wantFits, preview.Plan, preview.Candidates)
						}
						if mode != "manual" && (preview.SelectedNode != nil) != wantFits {
							t.Fatalf("automatic selection=%+v, want fit=%v", preview.SelectedNode, wantFits)
						}
						if mode != "manual" && (len(preview.Candidates) != 1 || preview.Candidates[0].OK != wantFits || (!wantFits && preview.Candidates[0].Reason.Code == "")) {
							t.Fatalf("automatic candidates=%+v, want fit=%v", preview.Candidates, wantFits)
						}
						before, err := w.Store.ListDeployments()
						if err != nil || len(before) != baseRows {
							t.Fatalf("preview wrote deployment rows: %+v %v", before, err)
						}
						created, err := svc.Create(context.Background(), req)
						wantCreated := wantFits || mode == "manual"
						if (err == nil) != wantCreated || (created != nil) != wantCreated {
							t.Fatalf("create=%+v err=%v, want created=%v", created, err, wantCreated)
						}
						if !wantCreated && deployment.KindOf(err) != deployment.KindConflict {
							t.Fatalf("room refusal has wrong kind: %v", err)
						}
						if wantCreated {
							baseRows++
							stored, err := w.Store.GetDeployment(created.ID)
							if err != nil || stored.NodeID != w.Node.ID || stored.VRAMLimitBytes != int64(req.VRAMLimitGiB)*fixtures.GiB {
								t.Fatalf("stored allocation=%+v err=%v", stored, err)
							}
							placement, err := planning.ParsePlacement(stored.Placement)
							if err != nil || placement.OperatorPinned() != (mode == "manual") {
								t.Fatalf("stored consent=%+v err=%v", placement, err)
							}
						}
						after, err := w.Store.ListDeployments()
						if err != nil || len(after) != baseRows {
							t.Fatalf("create left unexpected rows: %+v %v", after, err)
						}
					})
				}
			}
		}
	}
}

// Occupy every candidate in the documented hook between automatic selection
// and revalidation. This makes the race deterministic without timed sleeps.
func TestAutomaticCreateRechecksClaimsAfterPicking(t *testing.T) {
	w := fixtures.NewWorld(t)
	called := 0
	svc := service(t, w, func(uuid string) {
		called++
		if uuid != w.Node.UUID {
			t.Fatalf("picked %q", uuid)
		}
		dep := w.Deployment(t, "winner", 0)
		for index := range w.Node.GPUs {
			w.Instance(t, dep, fmt.Sprintf("winner-%d", index), proto.InstPending, "", index, 0)
		}
	})
	created, err := svc.Create(context.Background(), request(w))
	if called != 1 || created != nil || err == nil || deployment.KindOf(err) != deployment.KindConflict {
		t.Fatalf("picks=%d created=%+v err=%v", called, created, err)
	}
	rows, listErr := w.Store.ListDeployments()
	if listErr != nil || len(rows) != 1 || rows[0].Name != "winner" {
		t.Fatalf("refused create wrote rows: %+v %v", rows, listErr)
	}
}

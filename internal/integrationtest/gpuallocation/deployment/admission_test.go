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

type admissionRow struct {
	mode         string // auto, manual, or auto-echo: an automatic plan sent back with its cards
	partition    bool
	occupancy    string // free, whole, partition or foreign
	allowForeign bool
}

func (r admissionRow) name() string {
	return fmt.Sprintf("%s/partition-%v/%s/sharing-%v", r.mode, r.partition, r.occupancy, r.allowForeign)
}

// arrange occupies both cards and returns the request with the number of
// deployments that exist before it.
func (r admissionRow) arrange(t *testing.T, w fixtures.World) (dto.DeployRequest, int) {
	t.Helper()
	if err := w.Store.DB().Model(w.Node).Update("placement_share_with_foreign", r.allowForeign).Error; err != nil {
		t.Fatal(err)
	}
	existing := 0
	switch r.occupancy {
	case "whole", "partition":
		limit := int64(0)
		if r.occupancy == "partition" {
			limit = 16 * fixtures.GiB
		}
		holder := w.Deployment(t, "holder", limit)
		existing = 1
		for index := range w.Node.GPUs {
			w.Instance(t, holder, fmt.Sprintf("holder-%d", index), proto.InstRunning, "", index, 0)
		}
	case "foreign":
		held := fmt.Sprintf(`[{"pid":42,"name":"outside","memory_bytes":%d}]`, fixtures.ForeignHolderBytes)
		for i := range w.Node.GPUs {
			if err := w.Store.DB().Model(&w.Node.GPUs[i]).Update("foreign_processes", held).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	req := request(w)
	if r.partition {
		req.VRAMLimitGiB = 16
	}
	if r.mode == "manual" {
		req.PlacementMode = "manual"
		req.NodeUUID = w.Node.UUID
	}
	if r.mode != "auto" {
		req.Placement["gpus"] = []int{0}
	}
	return req, existing
}

type admissionExpectation struct {
	fits, previewOK, created bool
}

// A slice fits beside another slice, and beside a foreign holder when the node
// allows sharing or the operator chose the card. Automatic preview OK only
// validates the draft, while manual preview OK includes its node plan. Manual
// creation records intent even while the cards are busy.
func (r admissionRow) expectation() admissionExpectation {
	fits := r.occupancy == "free" || (r.occupancy == "partition" && r.partition) || (r.occupancy == "foreign" && r.partition && (r.allowForeign || r.mode == "manual"))
	return admissionExpectation{fits: fits, previewOK: fits || r.mode != "manual", created: fits || r.mode == "manual"}
}

// Exercise the public Evaluate/Create boundary. Manual creation records intent
// while cards are busy; automatic creation must find room now. An echoed
// automatic plan must not accidentally grant the consent of a manual pin.
func TestPlacementModeByAllocationAndOccupancy(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "auto-echo"} {
		for _, partition := range []bool{false, true} {
			for _, occupancy := range []string{"free", "whole", "partition", "foreign"} {
				for _, allowForeign := range []bool{false, true} {
					row := admissionRow{mode: mode, partition: partition, occupancy: occupancy, allowForeign: allowForeign}
					t.Run(row.name(), func(t *testing.T) {
						w := fixtures.NewWorld(t)
						req, rows := row.arrange(t, w)
						want := row.expectation()
						svc := service(t, w, nil)
						preview, err := svc.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: req})
						if err != nil || len(preview.Errors) != 0 || preview.OK != want.previewOK {
							t.Fatalf("preview OK=%v errors=%+v err=%v, want %+v plan=%+v candidates=%+v", preview.OK, preview.Errors, err, want, preview.Plan, preview.Candidates)
						}
						if row.mode != "manual" && (preview.SelectedNode != nil) != want.fits {
							t.Fatalf("automatic selection=%+v, want fit=%v", preview.SelectedNode, want.fits)
						}
						if row.mode != "manual" && (len(preview.Candidates) != 1 || preview.Candidates[0].OK != want.fits || (!want.fits && preview.Candidates[0].Reason.Code == "")) {
							t.Fatalf("automatic candidates=%+v, want fit=%v", preview.Candidates, want.fits)
						}
						before, err := w.Store.ListDeployments()
						if err != nil || len(before) != rows {
							t.Fatalf("preview wrote deployment rows: %+v %v", before, err)
						}
						created, err := svc.Create(context.Background(), req)
						if (err == nil) != want.created || (created != nil) != want.created {
							t.Fatalf("create=%+v err=%v, want created=%v", created, err, want.created)
						}
						if !want.created && deployment.KindOf(err) != deployment.KindConflict {
							t.Fatalf("room refusal has wrong kind: %v", err)
						}
						if want.created {
							rows++
							stored, err := w.Store.GetDeployment(created.ID)
							if err != nil || stored.NodeID != w.Node.ID || stored.VRAMLimitBytes != int64(req.VRAMLimitGiB)*fixtures.GiB {
								t.Fatalf("stored allocation=%+v err=%v", stored, err)
							}
							placement, err := planning.ParsePlacement(stored.Placement)
							if err != nil || placement.OperatorPinned() != (row.mode == "manual") {
								t.Fatalf("stored consent=%+v err=%v", placement, err)
							}
						}
						after, err := w.Store.ListDeployments()
						if err != nil || len(after) != rows {
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

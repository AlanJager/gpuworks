package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

type generationInferenceHandler struct {
	*recordingInferenceNodeHandler
	rejectNode string
}

func (h *generationInferenceHandler) BeforeCreate(ctx context.Context, req provider.InferenceNodeCreateRequest) error {
	if err := h.recordingInferenceNodeHandler.BeforeCreate(ctx, req); err != nil {
		return err
	}
	if req.NodeUUID == h.rejectNode {
		return errors.New("reservation rejected")
	}
	return nil
}

func TestGenerationResourceReservationsFollowPersistence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		rejectResource  bool
		rejectWrite     bool
		rejectPush      bool
		wantCompensated int
		wantInstances   int
	}{
		{name: "created", wantInstances: 2},
		{name: "follower reservation rejected", rejectResource: true, wantCompensated: 1},
		{name: "instance transaction rejected", rejectWrite: true, wantCompensated: 2},
		{name: "push failed after persistence", rejectPush: true, wantInstances: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, dep, nodes, hub := twoNodeGangFixture(t, "inference_generation")
			handler := &generationInferenceHandler{recordingInferenceNodeHandler: &recordingInferenceNodeHandler{cloud: true}}
			if tc.rejectResource {
				handler.rejectNode = nodes[1].UUID
			}
			if tc.rejectWrite {
				query := fmt.Sprintf("CREATE TRIGGER reject_generation BEFORE INSERT ON instances WHEN NEW.node_id = %d BEGIN SELECT RAISE(ABORT, 'instance write rejected'); END", nodes[1].ID)
				if err := st.DB().Exec(query).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.rejectPush {
				hub.pushFails = map[string]error{nodes[0].UUID: errors.New("node not connected")}
			}
			c := New(st, hub, nil)
			c.SetInferenceNodeLifecycleHandler(handler)
			err := c.reconcileDeployment(t.Context(), dep.ID)
			if wantErr := tc.rejectResource || tc.rejectWrite || tc.rejectPush; (err != nil) != wantErr {
				t.Fatalf("reconcile error = %v, want error %v", err, wantErr)
			}
			if len(handler.creates) != 2 {
				t.Fatalf("reservation requests = %+v, want both ranks", handler.creates)
			}
			for index, req := range handler.creates {
				if req.NodeUUID != nodes[index].UUID || !reflect.DeepEqual(req.GPUIndexes, []int{0}) || req.InstanceUUID == "" {
					t.Fatalf("rank %d reservation = %+v", index, req)
				}
			}
			if len(handler.failed) != tc.wantCompensated {
				t.Fatalf("compensated = %+v, want %d reservations", handler.failed, tc.wantCompensated)
			}
			for index, req := range handler.failed {
				if !reflect.DeepEqual(req, handler.creates[index]) {
					t.Fatalf("compensation changed reservation identity: %+v, want %+v", req, handler.creates[index])
				}
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != tc.wantInstances {
				t.Fatalf("instances = %+v, err=%v, want %d", instances, err, tc.wantInstances)
			}
			if tc.wantInstances == 0 && len(hub.updates) != 0 {
				t.Fatalf("unpersisted generation reached agents: %+v", hub.updates)
			}
			if tc.wantInstances > 0 && len(hub.updates[nodes[1].UUID]) == 0 {
				t.Fatal("persisted follower was not sent to its agent")
			}
		})
	}
}

package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type recordingInferenceNodeHandler struct {
	cloud    bool
	creates  []provider.InferenceNodeCreateRequest
	failed   []provider.InferenceNodeCreateRequest
	terminal []provider.InferenceNodeStopRequest
}

func (m *recordingInferenceNodeHandler) BeforeCreate(_ context.Context, req provider.InferenceNodeCreateRequest) error {
	if !m.cloud {
		return nil
	}
	m.creates = append(m.creates, req)
	return nil
}
func (m *recordingInferenceNodeHandler) OnCreateFailed(_ context.Context, req provider.InferenceNodeCreateRequest) error {
	m.failed = append(m.failed, req)
	return nil
}
func (m *recordingInferenceNodeHandler) RequiresStopConfirmation(context.Context, string) (bool, error) {
	return m.cloud, nil
}
func (m *recordingInferenceNodeHandler) AfterInstanceTerminal(_ context.Context, req provider.InferenceNodeStopRequest) error {
	m.terminal = append(m.terminal, req)
	return nil
}

func TestInferenceNodeHandlerReceivesAssignedGPUs(t *testing.T) {
	manager := &recordingInferenceNodeHandler{cloud: true}
	c := &Controller{inferenceNodeHandler: manager}
	node := &store.Node{UUID: "cloud-node"}
	if err := c.inferenceNodeHandlerBeforeCreate(context.Background(), node, []int{0}, "instance-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.inferenceNodeHandlerCreateFailed(context.Background(), node, []int{0}, "instance-1"); err != nil {
		t.Fatal(err)
	}
	want := []provider.InferenceNodeCreateRequest{{NodeUUID: node.UUID, InstanceUUID: "instance-1", GPUIndexes: []int{0}}}
	if !reflect.DeepEqual(manager.creates, want) || !reflect.DeepEqual(manager.failed, want) {
		t.Fatalf("assigned GPUs lost: creates=%+v failed=%+v", manager.creates, manager.failed)
	}
}

func TestFinalizeInstanceRemovalReleasesTerminalFailureBeforeDelete(t *testing.T) {
	s, err := store.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	node, err := s.RegisterNode("cloud-node", store.HashToken("token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := s.CreateInstance(&store.Instance{UUID: "failed-instance", NodeID: node.ID, State: proto.InstFailed, Message: "image pull failed"})
	if err != nil {
		t.Fatal(err)
	}
	handler := &recordingInferenceNodeHandler{cloud: true}
	c := &Controller{store: s, inferenceNodeHandler: handler}
	done, err := c.finalizeInstanceRemoval(context.Background(), *instance, true)
	if err != nil || !done {
		t.Fatalf("finalize = done:%v err:%v", done, err)
	}
	if len(handler.terminal) != 1 || handler.terminal[0].State != proto.InstFailed {
		t.Fatalf("terminal callback = %+v", handler.terminal)
	}
	if _, err := s.InstanceByUUID(instance.UUID); err != store.ErrNotFound {
		t.Fatalf("instance after finalization = err %v", err)
	}
}

func TestFinalizeInstanceRemovalWaitsForCloudConfirmation(t *testing.T) {
	s, err := store.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	node, err := s.RegisterNode("cloud-node", store.HashToken("token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := s.CreateInstance(&store.Instance{UUID: "running-instance", NodeID: node.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{store: s, inferenceNodeHandler: &recordingInferenceNodeHandler{cloud: true}}
	done, err := c.finalizeInstanceRemoval(context.Background(), *instance, true)
	if err != nil || done {
		t.Fatalf("finalize = done:%v err:%v, want wait", done, err)
	}
	if _, err := s.InstanceByUUID(instance.UUID); err != nil {
		t.Fatalf("instance was deleted before confirmation: %v", err)
	}
}

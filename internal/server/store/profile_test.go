package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInstanceMeasurementsRemainLocal(t *testing.T) {
	s, err := Open("file:store_profile_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.RegisterNode("profile-node", HashToken("token"), "profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&Node{}).Where("id = ?", node.ID).Updates(map[string]any{"gpu_vendor": "nvidia", "gpu_driver": "550"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&GPU{NodeID: node.ID, Index: 0, Vendor: "nvidia", ArchID: "sm_90", Name: "H100", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "profile-inst", DeploymentID: dep.ID, Engine: "vllm", Image: "vllm:test", ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"--max-model-len", "8192"}}, GPUIndexes: []int{0}})
	inst, err := s.CreateInstance(&Instance{UUID: "profile-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting, DesiredStateJSON: string(spec)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Measured: json.RawMessage(`{"KVCacheGiB":12.5,"Found":true}`)}); err != nil {
		t.Fatal(err)
	}

	// Repeated reports keep the latest diagnostic measurement on this instance.
	want := json.RawMessage(`{"KVCacheGiB":13.5,"Found":true}`)
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Measured: want}); err != nil {
		t.Fatal(err)
	}
	got, err := s.InstanceByUUID(inst.UUID)
	if err != nil || got.Measured != string(want) {
		t.Fatalf("instance measurement = %+v, err=%v", got, err)
	}
	var count int64
	if err := s.DB().Model(&Profile{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("runtime reports created %d reusable profiles", count)
	}
}

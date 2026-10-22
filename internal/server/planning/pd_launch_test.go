package planning

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

func TestPDRankLaunchShapesEachRankOfAMultiNodeUnit(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-nixl")
	unit := PDUnit{NodeUUID: "b", GPUIndexes: []int{0}, Followers: []PDRank{{NodeUUID: "c", GPUIndexes: []int{3}}}}
	pd := &PDPlacement{Mode: mode.Name, KVLoadFailure: pdmode.KVLoadRecompute, Roles: map[string]PDRole{
		"prefill": {TP: 1, Units: []PDUnit{{NodeUUID: "a"}}},
		"decode":  {TP: 2, Units: []PDUnit{unit}},
	}}
	ports := PDRankPorts{API: 30000, SideChannel: 30001, Master: 30003, LeaderAddress: "10.0.0.2"}

	leader := pd.RankLaunch(mode, "decode", unit, 0, DistTopology{NNodes: 2, NodeAddress: "10.0.0.2"}, ports)
	if leader.MasterAddr != "10.0.0.2" || leader.MasterPort != 30003 || leader.APIPort != 30000 {
		t.Fatalf("leader meets on %s:%d, api %d", leader.MasterAddr, leader.MasterPort, leader.APIPort)
	}
	if leader.TP != 2 || leader.PP != 1 || !reflect.DeepEqual(leader.GPUIndexes, []int{0}) {
		t.Fatalf("leader parallelism tp=%d pp=%d gpus=%v", leader.TP, leader.PP, leader.GPUIndexes)
	}
	want := &deploy.PDLaunch{Mode: "vllm-nixl", Role: "decode", KVLoadFailure: "recompute", SideChannelHost: "10.0.0.2", SideChannelPort: 30001, RoleTP: map[string]int{"prefill": 1, "decode": 2}}
	if !reflect.DeepEqual(leader.PD, want) {
		t.Fatalf("leader PD launch = %+v, want %+v", leader.PD, want)
	}

	follower := pd.RankLaunch(mode, "decode", unit, 1, DistTopology{NNodes: 2, NodeRank: 1, Headless: true, NodeAddress: "10.0.0.3"},
		PDRankPorts{SideChannel: 30010, Master: 30003, LeaderAddress: "10.0.0.2"})
	if follower.MasterAddr != "10.0.0.2" || follower.APIPort != 0 || !reflect.DeepEqual(follower.GPUIndexes, []int{3}) {
		t.Fatalf("follower meets on %s, api %d, gpus %v", follower.MasterAddr, follower.APIPort, follower.GPUIndexes)
	}
	if follower.PD.SideChannelHost != "10.0.0.3" || follower.PD.SideChannelPort != 30010 {
		t.Fatalf("follower side channel %s:%d", follower.PD.SideChannelHost, follower.PD.SideChannelPort)
	}
}

func TestPDRankLaunchApplyDropsDecodeKVTiers(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-nixl")
	unit := PDUnit{NodeUUID: "b"}
	pd := &PDPlacement{Mode: mode.Name, Roles: map[string]PDRole{"prefill": {Units: []PDUnit{{NodeUUID: "a"}}}, "decode": {Units: []PDUnit{unit}}}}
	req := LaunchRequest{
		Placement:  Placement{GPUIndexes: []int{7}, TP: 4},
		Quick:      deploy.LaunchConfig{KVOffloadingSize: 8, MaxModelLen: 4096},
		MasterAddr: "stale", MasterPort: 29500,
	}
	pd.RankLaunch(mode, "decode", unit, 0, DistTopology{NNodes: 1, NodeAddress: "10.0.0.2"}, PDRankPorts{API: 30000, SideChannel: 30001}).Apply(&req)
	if req.Placement.TP != 1 || req.Placement.GPUIndexes != nil {
		t.Fatalf("placement = %+v", req.Placement)
	}
	if req.Quick.KVOffloadingSize != 0 || req.Quick.MaxModelLen != 4096 {
		t.Fatalf("decode quick = %+v", req.Quick)
	}
	if req.MasterAddr != "" || req.MasterPort != 0 {
		t.Fatalf("single-node rank keeps a collective endpoint %s:%d", req.MasterAddr, req.MasterPort)
	}
	if req.APIPort != 30000 || req.PD == nil || req.PD.Role != "decode" {
		t.Fatalf("api %d PD %+v", req.APIPort, req.PD)
	}
}

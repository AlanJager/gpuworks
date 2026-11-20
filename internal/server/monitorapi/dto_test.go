package monitorapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

var zeroTime = time.Time{}

func TestDeploymentPlacementSingleNodeKeepsStatedTPAndNodeNames(t *testing.T) {
	dep := monitor.DeploymentRef{TensorParallel: 4}
	instances := []monitor.InstanceRef{
		{NodeUUID: "n1", GPUIndexes: []int{0, 1, 2, 3}},
		{NodeUUID: "n2", GPUIndexes: []int{0, 1, 2, 3}},
	}
	nodeNames := map[string]string{"n1": "gpu3", "n2": "gpu4"}
	want := dto.MonitorPlacement{Nodes: []string{"gpu3", "gpu4"}, NNodes: 1, GPUsPerNode: 4, TensorParallel: 4}
	if got := deploymentPlacement(dep, instances, nodeNames); !reflect.DeepEqual(got, want) {
		t.Fatalf("placement = %+v, want %+v", got, want)
	}
}

// A 4-node group with one card per node (4 DGX Spark running TP4): the
// placement says 4 nodes x 1 card, in rank order whatever order the
// instances come in, and states no TP the platform was not told.
func TestDeploymentPlacementMultiNodeIsInRankOrder(t *testing.T) {
	instances := []monitor.InstanceRef{
		{NodeUUID: "u4", GPUIndexes: []int{0}, DistGroup: "g", NodeRank: 3, NNodes: 4},
		{NodeUUID: "u3", GPUIndexes: []int{0}, DistGroup: "g", NodeRank: 2, NNodes: 4},
		{NodeUUID: "u2", GPUIndexes: []int{0}, DistGroup: "g", NodeRank: 1, NNodes: 4},
		{NodeUUID: "u1", GPUIndexes: []int{0}, DistGroup: "g", NodeRank: 0, NNodes: 4},
	}
	names := map[string]string{"u1": "gb10-1", "u2": "gb10-2", "u3": "gb10-3", "u4": "gb10-4"}
	want := dto.MonitorPlacement{Nodes: []string{"gb10-1", "gb10-2", "gb10-3", "gb10-4"}, NNodes: 4, GPUsPerNode: 1}
	if got := deploymentPlacement(monitor.DeploymentRef{}, instances, names); !reflect.DeepEqual(got, want) {
		t.Fatalf("placement = %+v, want %+v", got, want)
	}
}

// Several multi-node replicas list one replica's ranks together, replicas in
// the order they first appear, not every rank 0 before every rank 1.
func TestDeploymentPlacementKeepsReplicasTogether(t *testing.T) {
	instances := []monitor.InstanceRef{
		{NodeUUID: "b1", GPUIndexes: []int{0, 1}, DistGroup: "B", NodeRank: 1},
		{NodeUUID: "a1", GPUIndexes: []int{0, 1}, DistGroup: "A", NodeRank: 1},
		{NodeUUID: "b0", GPUIndexes: []int{0, 1}, DistGroup: "B", NodeRank: 0},
		{NodeUUID: "a0", GPUIndexes: []int{0, 1}, DistGroup: "A", NodeRank: 0},
	}
	got := deploymentPlacement(monitor.DeploymentRef{TensorParallel: 4}, instances, nil)
	want := dto.MonitorPlacement{Nodes: []string{"b0", "b1", "a0", "a1"}, NNodes: 2, GPUsPerNode: 2, TensorParallel: 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placement = %+v, want %+v", got, want)
	}
}

// A group missing a rank still reports the node count its instances state.
func TestDeploymentPlacementUsesStatedNodeCountWhenARankIsMissing(t *testing.T) {
	instances := []monitor.InstanceRef{{NodeUUID: "n1", GPUIndexes: []int{0}, DistGroup: "g", NodeRank: 0, NNodes: 3}}
	if got := deploymentPlacement(monitor.DeploymentRef{}, instances, nil); got.NNodes != 3 {
		t.Fatalf("nnodes = %d, want 3", got.NNodes)
	}
}

// A PD deployment's roles sit on their own nodes; the placement lists them all
// once each and carries no TP when the roles state different degrees (the
// adapter leaves TensorParallel zero then).
func TestDeploymentPlacementPDRolesListEveryNodeOnce(t *testing.T) {
	instances := []monitor.InstanceRef{
		{NodeUUID: "p", GPUIndexes: []int{0, 1}, Role: "prefill"},
		{NodeUUID: "d", GPUIndexes: []int{0, 1, 2, 3}, Role: "decode"},
		{NodeUUID: "d", GPUIndexes: []int{4, 5, 6, 7}, Role: "decode"},
	}
	names := map[string]string{"p": "gpu-p", "d": "gpu-d"}
	want := dto.MonitorPlacement{Nodes: []string{"gpu-p", "gpu-d"}, NNodes: 1, GPUsPerNode: 4}
	if got := deploymentPlacement(monitor.DeploymentRef{}, instances, names); !reflect.DeepEqual(got, want) {
		t.Fatalf("placement = %+v, want %+v", got, want)
	}
}

func TestDeploymentPlacementNoInstancesIsEmptyButNeverNull(t *testing.T) {
	got := deploymentPlacement(monitor.DeploymentRef{}, nil, nil)
	if got.Nodes == nil || len(got.Nodes) != 0 || got.NNodes != 0 || got.GPUsPerNode != 0 || got.TensorParallel != 0 {
		t.Fatalf("placement = %+v, want an empty placement with a non-nil node list", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"nodes":[],"nnodes":0,"gpus_per_node":0}` {
		t.Fatalf("wire = %s", raw)
	}
}

func TestDeploymentPlacementFallsBackToUUIDWhenNodeUnnamed(t *testing.T) {
	instances := []monitor.InstanceRef{{NodeUUID: "node-uuid-1", GPUIndexes: []int{0}}}
	got := deploymentPlacement(monitor.DeploymentRef{}, instances, map[string]string{})
	if !reflect.DeepEqual(got.Nodes, []string{"node-uuid-1"}) {
		t.Fatalf("nodes = %v", got.Nodes)
	}
}

// sleeper is a running instance whose engine reports a sleep level: asleep by
// the level and by the gateway's judgement alike.
func sleeper(level int) monitor.InstanceRef {
	return monitor.InstanceRef{State: proto.InstRunning, SleepLevel: level, Asleep: true}
}

// The monitor shows the list's state in its own vocabulary and derives
// nothing: every state the list shows maps to exactly one.
func TestDeploymentStateMapsTheListState(t *testing.T) {
	for list, want := range map[string]string{
		"pending": "starting", "pulling": "starting", "starting": "starting",
		"running": "running", "offline": "offline", "failed": "failed",
		"stopped": "stopped", "deleting": "stopped",
		"sleeping": "asleep", "asleep": "asleep",
		"": "stopped",
	} {
		if got := deploymentState(monitor.DeploymentRef{State: list}); got != want {
			t.Errorf("deploymentState(%q) = %q, want %q", list, got, want)
		}
	}
}

func TestReplicaStatePerInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst monitor.InstanceRef
		want string
	}{
		{"failed", monitor.InstanceRef{State: proto.InstFailed}, "failed"},
		{"asleep", sleeper(2), "asleep"},
		{"ordered to sleep, not carried out yet", monitor.InstanceRef{State: proto.InstRunning, Asleep: true}, "asleep"},
		{"running", monitor.InstanceRef{State: proto.InstRunning}, "running"},
		{"starting", monitor.InstanceRef{State: proto.InstOptimizing}, "starting"},
		{"stopped", monitor.InstanceRef{State: proto.InstStopped}, "stopped"},
		{"unknown state falls back to stopped", monitor.InstanceRef{State: "weird"}, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicaState(tc.inst); got != tc.want {
				t.Fatalf("replicaState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCountReadyReplicasExcludesHeadless(t *testing.T) {
	instances := []monitor.InstanceRef{
		{State: proto.InstRunning},
		{State: proto.InstRunning, Headless: true},
		{State: proto.InstFailed},
	}
	if got := monitor.ReadyReplicas(monitor.DeploymentRef{}, instances); got != 1 {
		t.Fatalf("countReadyReplicas = %d, want 1 (the headless follower must not count)", got)
	}
}

func TestCollectedFloatPtr(t *testing.T) {
	if collectedFloatPtr(false, 42) != nil {
		t.Fatal("collectedFloatPtr(false, ...) must be nil")
	}
	got := collectedFloatPtr(true, 42)
	if got == nil || *got != 42 {
		t.Fatalf("collectedFloatPtr(true, 42) = %v", got)
	}
}

func TestTimeMillisZeroTime(t *testing.T) {
	if got := timeMillis(zeroTime); got != 0 {
		t.Fatalf("timeMillis(zero) = %d, want 0", got)
	}
	at := baseTime
	if got := timeMillis(at); got != at.UnixMilli() {
		t.Fatalf("timeMillis = %d", got)
	}
}

func TestLastPointsTailsLongSeries(t *testing.T) {
	pts := make([]tsdb.Point, 20)
	for i := range pts {
		pts[i] = tsdb.Point{TimeMS: int64(i), Value: float64(i)}
	}
	got := lastPoints(pts, sparkPoints)
	if len(got) != sparkPoints {
		t.Fatalf("lastPoints returned %d points, want %d", len(got), sparkPoints)
	}
	if got[0][0] != float64(20-sparkPoints) || got[len(got)-1][0] != 19 {
		t.Fatalf("lastPoints = %+v, want the trailing %d timestamps", got, sparkPoints)
	}
}

func TestLastPointsShorterThanNReturnsAll(t *testing.T) {
	pts := []tsdb.Point{{TimeMS: 0, Value: 1}, {TimeMS: 1, Value: 2}}
	got := lastPoints(pts, sparkPoints)
	if len(got) != 2 {
		t.Fatalf("lastPoints = %+v, want both points unchanged", got)
	}
}

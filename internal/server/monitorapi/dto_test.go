package monitorapi

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

var zeroTime = time.Time{}

func TestDeploymentPlacementCombinesTPAndNodeNames(t *testing.T) {
	dep := monitor.DeploymentRef{}
	instances := []monitor.InstanceRef{
		{NodeUUID: "n1", GPUIndexes: []int{0, 1}},
		{NodeUUID: "n2", GPUIndexes: []int{0, 1, 2, 3}},
	}
	nodeNames := map[string]string{"n1": "gpu3", "n2": "gpu4"}
	if got := deploymentPlacement(dep, instances, nodeNames); got != "TP4 · gpu3, gpu4" {
		t.Fatalf("placement = %q", got)
	}
}

func TestDeploymentPlacementNoInstancesIsEmpty(t *testing.T) {
	if got := deploymentPlacement(monitor.DeploymentRef{}, nil, nil); got != "" {
		t.Fatalf("placement = %q, want empty for a deployment with no replicas yet", got)
	}
}

func TestDeploymentPlacementFallsBackToUUIDWhenNodeUnnamed(t *testing.T) {
	instances := []monitor.InstanceRef{{NodeUUID: "node-uuid-1", GPUIndexes: []int{0}}}
	if got := deploymentPlacement(monitor.DeploymentRef{}, instances, map[string]string{}); got != "TP1 · node-uuid-1" {
		t.Fatalf("placement = %q", got)
	}
}

func TestDeploymentStateRollup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []monitor.InstanceRef
		want      string
	}{
		{"no instances", nil, "stopped"},
		{"one starting", []monitor.InstanceRef{{State: proto.InstPulling}}, "starting"},
		{"one running awake", []monitor.InstanceRef{{State: proto.InstRunning}}, "running"},
		{"one running asleep", []monitor.InstanceRef{{State: proto.InstRunning, SleepLevel: 1}}, "asleep"},
		{"mixed asleep and awake running counts as running", []monitor.InstanceRef{
			{State: proto.InstRunning, SleepLevel: 1}, {State: proto.InstRunning},
		}, "running"},
		{"any failed outranks running", []monitor.InstanceRef{
			{State: proto.InstRunning}, {State: proto.InstFailed},
		}, "failed"},
		{"stopped only", []monitor.InstanceRef{{State: proto.InstStopped}}, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentState(tc.instances); got != tc.want {
				t.Fatalf("deploymentState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplicaStatePerInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst monitor.InstanceRef
		want string
	}{
		{"failed", monitor.InstanceRef{State: proto.InstFailed}, "failed"},
		{"asleep", monitor.InstanceRef{State: proto.InstRunning, SleepLevel: 2}, "asleep"},
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
	if got := countReadyReplicas(instances); got != 1 {
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

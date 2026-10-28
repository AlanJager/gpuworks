package app

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// automationFixture is one node with one 24 GiB GPU and a sleepable vLLM
// deployment whose one instance runs on it, behind the real adapter.
func automationFixture(t *testing.T) (*automationAdapter, *store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:automation_adapter_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("auto-node", store.HashToken("token"), "auto-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 24 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto", Name: "auto", WeightBytes: 2 << 30, Dtype: "bfloat16"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "auto", ModelID: model.ID, NodeID: node.ID, ServedName: "auto", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "auto-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "auto-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	adapter := &automationAdapter{store: st, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return adapter, st, dep
}

func intentOf(t *testing.T, st *store.Store, uuid string) int {
	t.Helper()
	in, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return in.SleepIntent()
}

// The adapter's sleep is an operator's deployment-wide order; idle sleep
// sees the deployment awake before and asleep after.
func TestAutomationAdapterSleepsAndReportsAwake(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	if err := st.SetDeploymentIdleSleep(dep.ID, 900, 2); err != nil {
		t.Fatal(err)
	}
	idle, err := adapter.IdleDeployments()
	if err != nil || len(idle) != 1 || !idle[0].Awake || idle[0].AfterSec != 900 || idle[0].Level != 2 {
		t.Fatalf("idle deployments = %+v err=%v", idle, err)
	}
	if err := adapter.Sleep(dep.ID, 1, false); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if got := intentOf(t, st, "auto-1"); got != 1 {
		t.Fatalf("intent after the automation's sleep = %d, want 1", got)
	}
	if got, _ := st.GetDeployment(dep.ID); got.SleepHeld {
		t.Fatal("an idle sleep is held: a request would not wake it")
	}
	if err := adapter.Sleep(dep.ID, 2, true); err != nil {
		t.Fatalf("held sleep: %v", err)
	}
	if got, _ := st.GetDeployment(dep.ID); !got.SleepHeld {
		t.Fatal("a schedule's sleep is not held")
	}
	if idle, _ := adapter.IdleDeployments(); len(idle) != 1 || idle[0].Awake {
		t.Fatalf("an ordered sleep still reads awake: %+v", idle)
	}
}

// With host memory reported too small for the weights, an L1 sleep is
// refused with its code and nothing changes.
func TestAutomationAdapterRefusesL1WhenTheHostCannotHoldTheWeights(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	metrics, err := tsdb.OpenSQLite(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })
	node, _ := st.NodeByID(dep.NodeID)
	now := time.Now().UnixMilli()
	if err := metrics.WriteBatch([]tsdb.Sample{
		{Metric: "gpuworks_node_mem_total_bytes", Labels: map[string]string{"node": node.UUID}, Value: float64(8 << 30), TimeMS: now},
		{Metric: "gpuworks_node_mem_used_bytes", Labels: map[string]string{"node": node.UUID}, Value: float64(7 << 30), TimeMS: now},
	}); err != nil {
		t.Fatal(err)
	}
	adapter.metrics = metrics
	err = adapter.Sleep(dep.ID, 1, true)
	coded, ok := err.(interface{ Code() string })
	if !ok || coded.Code() != "SLEEP_HOST_MEMORY_INSUFFICIENT" {
		t.Fatalf("L1 onto a full host = %v, want SLEEP_HOST_MEMORY_INSUFFICIENT", err)
	}
	if got := intentOf(t, st, "auto-1"); got != 0 {
		t.Fatalf("a refused sleep changed the intent to %d", got)
	}
	if err := adapter.Sleep(dep.ID, 2, true); err != nil || intentOf(t, st, "auto-1") != 2 {
		t.Fatalf("L2 onto a full host: err=%v intent=%d", err, intentOf(t, st, "auto-1"))
	}
}

// A scheduled wake that does not fit leaves the sleeper waiting; once the card
// frees up, the waiting wake wakes it; schedules read back as the automation
// sees them.
func TestAutomationAdapterWakeWaitsAndWakesWhenItFits(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "auto-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "other", ModelID: dep.ModelID, NodeID: dep.NodeID, ServedName: "other", Engine: "vllm", State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "other-1", DesiredState: "running"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "other-1", DeploymentID: other.ID, NodeID: dep.NodeID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Wake(dep.ID)
	if err != nil || len(result.Woken) != 0 || len(result.Waiting) != 1 || result.Waiting[0] != "auto-1" {
		t.Fatalf("wake onto a taken card = %+v err=%v", result, err)
	}
	waits, err := adapter.WaitingWakes()
	if err != nil || len(waits) != 1 || waits[0].DeploymentID != dep.ID {
		t.Fatalf("waiting wakes = %+v err=%v", waits, err)
	}
	if woken, err := adapter.WakeWaiting(dep.ID, waits[0].Instances); err != nil || len(woken) != 0 {
		t.Fatalf("woke while the card is taken: %v err=%v", woken, err)
	}
	if err := st.SetInstanceState("other-1", proto.InstStopped, "stopped"); err != nil {
		t.Fatal(err)
	}
	woken, err := adapter.WakeWaiting(dep.ID, waits[0].Instances)
	if err != nil || len(woken) != 1 || intentOf(t, st, "auto-1") != 0 {
		t.Fatalf("waiting wake after the card freed = %v err=%v intent=%d", woken, err, intentOf(t, st, "auto-1"))
	}
	if waits, _ := adapter.WaitingWakes(); len(waits) != 0 {
		t.Fatalf("still waiting after the wake: %+v", waits)
	}

	sc := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: store.ScheduleActionSleep, Weekdays: automation.AllWeekdays, Minute: 60, Timezone: "Asia/Shanghai", SleepLevel: 2, Enabled: true}
	if err := st.CreateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	broken := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: store.ScheduleActionWake, Weekdays: 1, Minute: 0, Timezone: "Nowhere/Never", Enabled: true}
	if err := st.CreateLifecycleSchedule(broken); err != nil {
		t.Fatal(err)
	}
	schedules, err := adapter.Schedules()
	if err != nil || len(schedules) != 1 || schedules[0].Location.String() != "Asia/Shanghai" || schedules[0].LastFiredAt.IsZero() {
		t.Fatalf("schedules = %+v err=%v (an unknown zone is skipped)", schedules, err)
	}
}

// A replica schedule goes through the API's own scaling: a single-node
// deployment records the count, and a multi-node deployment whose replicas the
// operator placed by hand is refused with the code the failure event carries,
// leaving the deployment as it was.
func TestAutomationAdapterScalesThroughTheAPIsPath(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	adapter.scale = api.ReplicaScaler(api.Deps{Store: st})
	if err := adapter.ScaleReplicas(dep.ID, 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.Replicas != 3 {
		t.Fatalf("replicas = %d, want 3", got.Replicas)
	}

	multi, err := st.CreateDeployment(&store.Deployment{Name: "multi", ServedName: "multi", Engine: "vllm", State: store.DeployActive, NNodes: 2, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.ScaleReplicas(multi.ID, 2)
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != messages.ErrorScheduleReplicasManualPlacement {
		t.Fatalf("manual placement: err = %v, want a product error %s", err, messages.ErrorScheduleReplicasManualPlacement)
	}
	if got, _ := st.GetDeployment(multi.ID); got.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused order, want 1", got.Replicas)
	}
}

package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// The full parse a first health check collects, and the partial re-parse that
// arrives hours later once the startup summary has slid out of the log tail —
// field values observed on a real node (vLLM 0.25.1, two replicas).
const measuredFullParse = `{"ModelWeightsGiB":0.93,"KVCacheGiB":18.09,"KVCacheTokens":1580976,"GraphCaptureGiB":0.35,"MaxConcurrency":48.25,"NumGPUBlocks":0,"Found":true}`
const measuredGraphOnlyReparse = `{"ModelWeightsGiB":0,"KVCacheGiB":0,"KVCacheTokens":0,"GraphCaptureGiB":0.35,"MaxConcurrency":0,"NumGPUBlocks":0,"Found":true}`

func TestMeasuredAfterReportKeepsFieldsTheReparseMissed(t *testing.T) {
	inst := &Instance{ContainerID: "c1", Measured: measuredFullParse}
	got := measuredAfterReport(inst, proto.InstanceStatus{ContainerID: "c1", Measured: []byte(measuredGraphOnlyReparse)})
	var m enginelog.Measured
	if err := unmarshalMeasured(got, &m); err != nil {
		t.Fatalf("merged json: %v", err)
	}
	if m.ModelWeightsGiB != 0.93 || m.KVCacheGiB != 18.09 || m.KVCacheTokens != 1580976 || m.MaxConcurrency != 48.25 {
		t.Fatalf("reparse erased earlier fields: %+v", m)
	}
	if m.GraphCaptureGiB != 0.35 || !m.Found {
		t.Fatalf("reparse fields not applied: %+v", m)
	}
}

func TestMeasuredAfterReportLetsFreshValuesWin(t *testing.T) {
	inst := &Instance{ContainerID: "c1", Measured: measuredFullParse}
	// A wake/restart re-parse of the same container finds every line again,
	// with new figures the new startup produced.
	fresh := `{"ModelWeightsGiB":0.94,"KVCacheGiB":16.2,"KVCacheTokens":1400000,"GraphCaptureGiB":0.3,"MaxConcurrency":42,"NumGPUBlocks":7,"Found":true}`
	got := measuredAfterReport(inst, proto.InstanceStatus{ContainerID: "c1", Measured: []byte(fresh)})
	var m enginelog.Measured
	if err := unmarshalMeasured(got, &m); err != nil {
		t.Fatalf("merged json: %v", err)
	}
	if m.ModelWeightsGiB != 0.94 || m.KVCacheGiB != 16.2 || m.GraphCaptureGiB != 0.3 || m.NumGPUBlocks != 7 {
		t.Fatalf("fresh values did not replace stored ones: %+v", m)
	}
}

func TestMeasuredAfterReportReplacesAcrossContainerGenerations(t *testing.T) {
	inst := &Instance{ContainerID: "c1", Measured: measuredFullParse}
	// A relaunched instance gets a new container; its parse is the only truth
	// about the current startup, even when it found fewer fields.
	got := measuredAfterReport(inst, proto.InstanceStatus{ContainerID: "c2", Measured: []byte(measuredGraphOnlyReparse)})
	var m enginelog.Measured
	if err := unmarshalMeasured(got, &m); err != nil {
		t.Fatalf("merged json: %v", err)
	}
	if m.ModelWeightsGiB != 0 || m.KVCacheGiB != 0 {
		t.Fatalf("previous container's figures survived into the new generation: %+v", m)
	}
	if m.GraphCaptureGiB != 0.35 {
		t.Fatalf("incoming fields dropped: %+v", m)
	}
}

func TestMeasuredAfterReportFallsBackToIncoming(t *testing.T) {
	for name, inst := range map[string]*Instance{
		"no stored measured": {ContainerID: "c1"},
		"unparseable stored": {ContainerID: "c1", Measured: "not json"},
	} {
		got := measuredAfterReport(inst, proto.InstanceStatus{ContainerID: "c1", Measured: []byte(measuredGraphOnlyReparse)})
		if got != measuredGraphOnlyReparse {
			t.Fatalf("%s: got %s, want incoming verbatim", name, got)
		}
	}
	// A report without a container ID cannot be tied to a generation either.
	inst := &Instance{ContainerID: "c1", Measured: measuredFullParse}
	if got := measuredAfterReport(inst, proto.InstanceStatus{Measured: []byte(measuredGraphOnlyReparse)}); got != measuredGraphOnlyReparse {
		t.Fatalf("report without container id: got %s, want incoming verbatim", got)
	}
}

func TestUpdateInstanceStatusPreservesMeasuredAcrossResync(t *testing.T) {
	s, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.CreateInstance(&Instance{UUID: "inst-1", DeploymentID: 1, NodeID: 1}); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	report := func(containerID, measured string) {
		t.Helper()
		st := proto.InstanceStatus{UUID: "inst-1", State: proto.InstRunning, ContainerID: containerID}
		if measured != "" {
			st.Measured = []byte(measured)
		}
		if err := s.UpdateInstanceStatus(st); err != nil {
			t.Fatalf("update status: %v", err)
		}
	}

	report("c1", measuredFullParse)
	// Hours of periodic engine logs later, a resync re-probe re-parses a log
	// tail that no longer reaches the startup summary.
	report("c1", measuredGraphOnlyReparse)
	inst, err := s.InstanceByUUID("inst-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	var m enginelog.Measured
	if err := unmarshalMeasured(inst.Measured, &m); err != nil {
		t.Fatalf("stored measured: %v", err)
	}
	if m.ModelWeightsGiB != 0.93 || m.KVCacheGiB != 18.09 || m.GraphCaptureGiB != 0.35 {
		t.Fatalf("partial reparse overwrote the stored breakdown: %+v", m)
	}

	// A report from a relaunched container is a new generation and replaces.
	report("c2", measuredGraphOnlyReparse)
	inst, err = s.InstanceByUUID("inst-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := unmarshalMeasured(inst.Measured, &m); err != nil {
		t.Fatalf("stored measured: %v", err)
	}
	if m.ModelWeightsGiB != 0 || m.KVCacheGiB != 0 || m.GraphCaptureGiB != 0.35 {
		t.Fatalf("previous container's figures survived relaunch: %+v", m)
	}

	// A status report without measured (progress heartbeat) leaves the column.
	report("c2", "")
	inst, err = s.InstanceByUUID("inst-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if inst.Measured == "" {
		t.Fatal("heartbeat without measured erased the column")
	}
}

func unmarshalMeasured(s string, m *enginelog.Measured) error {
	return json.Unmarshal([]byte(s), m)
}

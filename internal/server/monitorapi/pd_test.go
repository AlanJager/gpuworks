package monitorapi

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// A finished PD request's stage, null where unknown; an ordinary request
// has none.
func TestRequestDTOCarriesThePDStage(t *testing.T) {
	r := lookupRecord("r", 7, [16]byte{1})
	if got := recordDTO(r, nil); got.PD != nil {
		t.Fatalf("plain request pd = %+v", got.PD)
	}
	r.PD = &uxrecord.PDStage{PrefillInstance: "p-1", PrefillMS: 795, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: 640, Fallback: ""}
	pd := recordDTO(r, nil).PD
	if pd == nil || pd.PrefillInstanceUUID != "p-1" || *pd.PrefillMS != 795 || pd.PrefillQueueMS != nil || *pd.PrefillComputeMS != 640 {
		t.Fatalf("pd = %+v", pd)
	}
}

// In flight, a request in its stage has no stage time yet; once dispatched
// the stage time is dispatch less the stage's start.
func TestLiveDTOCarriesThePDStage(t *testing.T) {
	l := liveRequest("w", 1, uxrecord.Unknown, uxrecord.Unknown, uxrecord.Unknown)
	if liveDTO(l, nil).PD != nil {
		t.Fatal("a plain live request has a pd stage")
	}
	l.PDPrefillInstance, l.PDPrefillStartMS = "p-1", 20
	pd := liveDTO(l, nil).PD
	if pd == nil || pd.PrefillInstanceUUID != "p-1" || pd.PrefillMS != nil {
		t.Fatalf("in the stage = %+v", pd)
	}
	l.DispatchMS = 815
	if pd := liveDTO(l, nil).PD; pd.PrefillMS == nil || *pd.PrefillMS != 795 {
		t.Fatalf("after the stage = %+v", pd)
	}
}

// The PD summary's KV transfer: values in ms and MB when the engine exports
// them, null with exposed false when it does not.
func TestPDDTOCarriesTheKVTransfer(t *testing.T) {
	out := pdDTO(monitor.PDWindow{IsPD: true, TransferExposed: true, HasTransfer: true, TransferAvgSec: 1.2942, HasTransferP95: true,
		TransferP95Sec: 1.878, TransferAvgBytes: 217.06e6, TransferBytesPerSec: 167.72e6, TransferFailed: 1})
	kv := out.KVTransfer
	if !kv.Exposed || *kv.AvgMs != 1294.2 || *kv.P95Ms != 1878 || *kv.AvgMB != 217.1 || *kv.MBPerSec != 167.7 || *kv.Failed != 1 || *kv.Expired != 0 {
		t.Fatalf("kv = %+v", kv)
	}
	none := pdDTO(monitor.PDWindow{IsPD: true}).KVTransfer
	if none.Exposed || none.AvgMs != nil || none.Failed != nil {
		t.Fatalf("not exposed = %+v", none)
	}
}

// The prefill stage's p95 is read off the request store's stage histogram
// row; another row's cells do not count, and no cells leave it null.
func TestPrefillStageP95ComesFromTheStageRow(t *testing.T) {
	stage := uxrecord.LatencyRow(string(uxrecord.SegmentPDPrefill))
	src := &fakeRequests{grids: map[string][]uxrecord.Cell{uxrecord.GridSegments: {
		{X: stage, Y: uxrecord.LatencyBin(800), N: 19},
		{X: stage, Y: uxrecord.LatencyBin(1900), N: 1},
		{X: uxrecord.LatencyRow(string(uxrecord.SegmentGateway)), Y: uxrecord.LatencyBin(90000), N: 50},
	}}}
	svc := requestsService(src)
	pd := svc.withPrefillStageP95(context.Background(), 7, requestsNow, &dto.MonitorPD{})
	if pd.PrefillStageP95Ms == nil || *pd.PrefillStageP95Ms != round1(uxrecord.LatencyBinMS(uxrecord.LatencyBin(800))) {
		t.Fatalf("p95 = %v", pd.PrefillStageP95Ms)
	}
	empty := requestsService(&fakeRequests{}).withPrefillStageP95(context.Background(), 7, requestsNow, &dto.MonitorPD{})
	if empty.PrefillStageP95Ms != nil {
		t.Fatal("p95 without cells")
	}
	if requestsService(src).withPrefillStageP95(context.Background(), 7, requestsNow, nil) != nil {
		t.Fatal("a non-PD deployment got a summary")
	}
}

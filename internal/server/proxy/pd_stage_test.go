package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// pdRecord sends one chat request through a PD harness and returns its
// record.
func pdRecord(t *testing.T, h *pdHarness) uxrecord.Request {
	t.Helper()
	records := make(chan uxrecord.Request, 2)
	h.proxy.SetRequestSink(func(r uxrecord.Request) { records <- r })
	rec := h.do(t, chatPath, pdChat)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	select {
	case r := <-records:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
		return uxrecord.Request{}
	}
}

// The sequential handoff's record names the prefill instance, how long the
// gateway waited for the stage (inside the dispatch) and what the prefill
// instance reported of its own queue and prefill; the decode instance is
// the record's instance.
func TestASequentialPDRecordCarriesItsPrefillStage(t *testing.T) {
	h := newPDHarness(t, 1, "")
	r := pdRecord(t, h)
	if r.InstanceUUID != "decode-0" || r.PD == nil {
		t.Fatalf("record = %+v", r)
	}
	pd := *r.PD
	if pd.PrefillInstance != "prefill-0" || pd.Fallback != "" || pd.PrefillQueueMS != 2 || pd.PrefillComputeMS != 640 {
		t.Fatalf("pd = %+v", pd)
	}
	if pd.PrefillMS < 0 || pd.PrefillMS > r.DispatchMS || pd.StartMS < 0 || pd.StartMS > r.DispatchMS || pd.SpanID.IsZero() {
		t.Fatalf("stage %d ms from %d ms, dispatch %d ms", pd.PrefillMS, pd.StartMS, r.DispatchMS)
	}
	// The gateway's share and the stage make up the dispatch.
	upToDispatch := 0.0
	for _, s := range uxrecord.SegmentsOf(r) {
		if s.Kind == uxrecord.SegmentGateway || s.Kind == uxrecord.SegmentPDPrefill {
			upToDispatch += s.MS
		}
	}
	if upToDispatch != float64(r.DispatchMS) {
		t.Fatalf("gateway + stage = %v, dispatch %d", upToDispatch, r.DispatchMS)
	}
}

// A prefill instance that fails sends the request to the decoder alone: the
// record says why and names no prefill instance; the time spent trying is
// still the stage.
func TestAPDFallbackIsOnTheRecord(t *testing.T) {
	h := newPDHarness(t, 1, "")
	h.prefills[0].mode = "500"
	r := pdRecord(t, h)
	if r.PD == nil || r.PD.Fallback != pdFallbackServerError || r.PD.PrefillInstance != "" || r.PD.PrefillMS < 0 ||
		r.PD.PrefillQueueMS != uxrecord.Unknown || r.PD.PrefillComputeMS != uxrecord.Unknown {
		t.Fatalf("pd = %+v", r.PD)
	}
}

// The concurrent handoff (SGLang) does not wait for the prefill: the record
// names the prefill instance and has no stage time.
func TestAConcurrentPDRecordHasNoStageTime(t *testing.T) {
	h := newPDHarnessMode(t, "sglang-nixl", 1, "")
	r := pdRecord(t, h)
	if r.PD == nil || r.PD.PrefillInstance != "prefill-0" || r.PD.PrefillMS != uxrecord.Unknown || r.PD.Fallback != "" {
		t.Fatalf("pd = %+v", r.PD)
	}
	for _, s := range uxrecord.SegmentsOf(r) {
		if s.Kind == uxrecord.SegmentPDPrefill {
			t.Fatalf("segments = %v", uxrecord.SegmentsOf(r))
		}
	}
}

// A request in its prefill stage is in flight: listed before any decode
// instance has it, with its prefill instance and no dispatch; it leaves the
// table with its reply.
func TestARequestInItsPrefillStageIsLive(t *testing.T) {
	h := newPDHarness(t, 1, "")
	release := make(chan struct{})
	entered := make(chan struct{})
	slow := h.prefills[0]
	h.hub.mu.Lock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		slow.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h.hub.routes["127.0.0.1:32000"] = strings.TrimPrefix(server.URL, "http://")
	h.hub.mu.Unlock()

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- h.do(t, chatPath, pdChat) }()
	<-entered
	time.Sleep(30 * time.Millisecond) // the stage is under way for a while
	live := h.proxy.LiveRequests(h.dep.ID, 10)
	close(release)
	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(live.Requests) != 1 || live.Waiting != 1 {
		t.Fatalf("live = %+v", live)
	}
	l := live.Requests[0]
	if l.PDPrefillInstance != "prefill-0" || l.DispatchMS != uxrecord.Unknown || l.PDPrefillStartMS < 0 || l.InstanceUUID != "" {
		t.Fatalf("live request = %+v", l)
	}
	if segs := uxrecord.LiveSegments(l); segs[len(segs)-1].Kind != uxrecord.SegmentPDPrefill {
		t.Fatalf("live segments = %v", segs)
	}
	if after := h.proxy.LiveRequests(h.dep.ID, 10); len(after.Requests) != 0 {
		t.Fatalf("still live after the reply: %+v", after)
	}
}

// A prefill instance's 4xx goes back to the client as it came; the request
// leaves the in-flight table and leaves no record, as before.
func TestAPrefillClientErrorLeavesNoLiveEntry(t *testing.T) {
	h := newPDHarness(t, 1, "")
	h.prefills[0].mode = "400"
	records := 0
	h.proxy.SetRequestSink(func(uxrecord.Request) { records++ })
	if rec := h.do(t, chatPath, pdChat); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	if live := h.proxy.LiveRequests(h.dep.ID, 10); len(live.Requests) != 0 || records != 0 {
		t.Fatalf("live %+v records %d", live, records)
	}
}

// A probe of a PD deployment takes the prefill stage but stays out of the
// in-flight table.
func TestAPDProbeIsNotLive(t *testing.T) {
	h := newPDHarness(t, 1, "")
	reply := h.proxy.Probe(context.Background(), h.dep.ID, chatPath, map[string]any{"messages": []map[string]any{{"role": "user", "content": "1"}}, "max_tokens": 1})
	if reply.HTTPStatus != http.StatusOK {
		t.Fatalf("probe = %+v", reply)
	}
	if live := h.proxy.LiveRequests(h.dep.ID, 10); len(live.Requests) != 0 {
		t.Fatalf("live = %+v", live)
	}
}

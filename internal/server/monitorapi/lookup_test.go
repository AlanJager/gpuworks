package monitorapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

const upstreamTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

func lookupRecord(id string, dep uint, trace tracecontext.TraceID) uxrecord.Request {
	return uxrecord.Request{ID: id, DeploymentID: dep, Trace: tracecontext.Span{Trace: trace}, ReceivedMS: requestsNow.UnixMilli() - 5000,
		DispatchMS: 1, FirstByteMS: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown,
		LastChunkMS: uxrecord.Unknown, EndMS: 30, MaxGapMS: uxrecord.Unknown, Status: 200, CachedTokens: uxrecord.Unknown,
		ReasoningTokens: uxrecord.Unknown, EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown, Weight: 1}
}

// A trace's requests come back across deployments with the deployments
// named: a deleted one by the name it left, marked deleted; one deleted
// before names were kept is listed without a name.
func TestLookupNamesTheDeploymentsOfATrace(t *testing.T) {
	trace, _ := tracecontext.ParseTraceID(upstreamTrace)
	src := &fakeRequests{
		lookups:  [][]uxrecord.Request{{lookupRecord("r2", 9, trace), lookupRecord("r1", 7, trace), lookupRecord("r0", 9, trace), lookupRecord("r3", 5, trace)}},
		depNames: map[uint]DeploymentName{7: {Name: "qwen"}, 5: {Name: "org/old", Deleted: true}},
		names:    map[uint]string{},
	}
	out, err := requestsService(src).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{TraceID: strings.ToUpper(upstreamTrace), FromMS: 1000, ToMS: 2000, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if want := []LookupQuery{{TraceID: trace, FromMS: 1000, ToMS: 2000, Limit: 20}}; !reflect.DeepEqual(src.lookupArgs, want) {
		t.Fatalf("asked %+v, want %+v", src.lookupArgs, want)
	}
	if out.Window != "requested" || out.FromMS != 1000 || out.ToMS != 2000 || out.AsOf != requestsNow.UnixMilli() {
		t.Fatalf("window = %+v", out)
	}
	if len(out.Requests) != 4 || out.Requests[0].TraceID != upstreamTrace || out.Requests[1].DeploymentID != 7 || out.Requests[0].Phase != "done" {
		t.Fatalf("requests = %+v", out.Requests)
	}
	if want := []dto.MonitorLookupDeployment{{ID: 9, Name: ""}, {ID: 7, Name: "qwen"}, {ID: 5, Name: "org/old", Deleted: true}}; !reflect.DeepEqual(out.Deployments, want) {
		t.Fatalf("deployments = %+v", out.Deployments)
	}
}

// Without a window, a trace id the gateway minted is searched around the
// time it carries; one that decodes to no plausible time, and a request id,
// are searched in the last hour.
func TestLookupChoosesTheWindow(t *testing.T) {
	minted := tracecontext.NewTraceID(requestsNow.Add(-3 * time.Hour))
	src := &fakeRequests{lookups: [][]uxrecord.Request{{lookupRecord("r", 7, minted)}}}
	out, err := requestsService(src).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{TraceID: minted.String()})
	if err != nil {
		t.Fatal(err)
	}
	at := requestsNow.Add(-3 * time.Hour)
	if out.Window != "trace_time" || out.FromMS != at.Add(-10*time.Minute).UnixMilli() || out.ToMS != at.Add(time.Minute).UnixMilli() || len(src.lookupArgs) != 1 {
		t.Fatalf("out = %+v, asked %+v", out, src.lookupArgs)
	}

	// Nothing around the decoded time: the last hour is searched next.
	src = &fakeRequests{lookups: [][]uxrecord.Request{nil, {lookupRecord("r", 7, minted)}}}
	out, err = requestsService(src).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{TraceID: minted.String()})
	if err != nil || out.Window != "default" || len(src.lookupArgs) != 2 || src.lookupArgs[1].FromMS != requestsNow.Add(-time.Hour).UnixMilli() || len(out.Requests) != 1 {
		t.Fatalf("fallback = %+v %v, asked %+v", out, err, src.lookupArgs)
	}

	// An id from far in the past or the future, and a request id: the last hour only.
	for _, q := range []dto.MonitorRequestLookupQuery{
		{TraceID: upstreamTrace},
		{TraceID: tracecontext.NewTraceID(requestsNow.Add(time.Hour)).String()},
		{TraceID: tracecontext.NewTraceID(requestsNow.Add(-9 * 24 * time.Hour)).String()},
		{RequestID: "  gw-1 "},
	} {
		src = &fakeRequests{}
		out, err = requestsService(src).LookupRequests(context.Background(), q)
		if err != nil || out.Window != "default" || len(src.lookupArgs) != 1 || out.FromMS != requestsNow.Add(-time.Hour).UnixMilli() || out.ToMS != requestsNow.UnixMilli() {
			t.Fatalf("%+v: out = %+v %v, asked %+v", q, out, err, src.lookupArgs)
		}
		if out.Requests == nil || out.Deployments == nil {
			t.Fatalf("empty answer lists are null: %+v", out)
		}
	}
	if src.lookupArgs[0].RequestID != "gw-1" || !src.lookupArgs[0].TraceID.IsZero() {
		t.Fatalf("request id lookup asked %+v", src.lookupArgs[0])
	}
}

func TestLookupRefusesBadQueriesAndPassesSourceErrors(t *testing.T) {
	for name, q := range map[string]dto.MonitorRequestLookupQuery{
		"no id":           {},
		"both":            {TraceID: upstreamTrace, RequestID: "x"},
		"short trace":     {TraceID: upstreamTrace[:30]},
		"zero trace":      {TraceID: strings.Repeat("0", 32)},
		"window too long": {RequestID: "x", FromMS: 1, ToMS: 2 + uxrecord.MaxLookupWindow.Milliseconds()},
	} {
		src := &fakeRequests{}
		if _, err := requestsService(src).LookupRequests(context.Background(), q); !errors.Is(err, errBadQuery) {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(src.lookupArgs) != 0 {
			t.Errorf("%s reached the store", name)
		}
	}
	boom := errors.New("disk on fire")
	if _, err := requestsService(&fakeRequests{lookupErr: boom}).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{RequestID: "x"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := requestsService(&fakeRequests{lookupErr: fmt.Errorf("%w: limit", ErrBadHistoryQuery)}).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{RequestID: "x"}); !errors.Is(err, errBadQuery) {
		t.Fatalf("a query the store refuses: err = %v", err)
	}
	out, err := New(Deps{Now: func() time.Time { return requestsNow }}).LookupRequests(context.Background(), dto.MonitorRequestLookupQuery{RequestID: "x"})
	if err != nil || len(out.Requests) != 0 {
		t.Fatalf("no source: %+v %v", out, err)
	}
}

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/monitorapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
)

// TestRequestSourceReadsTheRequestStore drives the adapter over a real store
// and a real ux.db: records the gateway hands over come back as history,
// grids and coverage, a cursor ux.db did not issue maps to the monitor's
// bad-query error, and key names come from the API keys.
func TestRequestSourceReadsTheRequestStore(t *testing.T) {
	st, err := store.Open("file:app_request_source?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dep, err := st.CreateDeployment(&store.Deployment{Name: "chat", ServedName: "chat-served", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIKey("chat-app", 1); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListAPIKeys()
	if err != nil || len(keys) == 0 {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	key := keys[len(keys)-1]

	path := filepath.Join(t.TempDir(), "ux.db")
	ux, err := uxstore.Open(path, uxstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	trace := tracecontext.NewTraceID(now)
	for i := range 3 {
		ux.Observe(uxrecord.Request{
			ID: "r" + string(rune('0'+i)), Trace: tracecontext.Span{Trace: trace}, ReceivedMS: now.Add(-time.Duration(i) * time.Minute).UnixMilli(),
			DeploymentID: dep.ID, APIKeyID: key.ID, Stream: true, Status: 200,
			DispatchMS: 1, FirstByteMS: 20, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: 30, LastChunkMS: 300, EndMS: 301,
			Usage: true, PromptTokens: 1000, CompletionTokens: 50, CachedTokens: 600, ReasoningTokens: uxrecord.Unknown,
			EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown, Weight: 1,
		})
	}
	// Close flushes; reopen to read what was written.
	if err := ux.Close(); err != nil {
		t.Fatal(err)
	}
	ux, err = uxstore.Open(path, uxstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ux.Close() })

	live := func(id uint, limit int) uxrecord.LiveSnapshot { return uxrecord.LiveSnapshot{Waiting: int(id)} }
	src := newRequestSource(st, live, ux)
	ctx := context.Background()

	if !src.DeploymentExists(dep.ID) || src.DeploymentExists(dep.ID+100) {
		t.Fatal("DeploymentExists does not follow the store")
	}
	if got := src.Live(4, 10); got.Waiting != 4 {
		t.Fatalf("live = %+v", got)
	}
	from, to := now.Add(-time.Hour).UnixMilli(), now.Add(time.Minute).UnixMilli()
	records, next, err := src.History(ctx, monitorapi.HistoryQuery{DeploymentID: dep.ID, FromMS: from, ToMS: to, Limit: 2})
	if err != nil || len(records) != 2 || next == "" || records[0].ID != "r0" {
		t.Fatalf("history = %+v next=%q err=%v", records, next, err)
	}
	if _, _, err := src.History(ctx, monitorapi.HistoryQuery{DeploymentID: dep.ID, Cursor: "not-a-cursor"}); !errors.Is(err, monitorapi.ErrBadHistoryQuery) {
		t.Fatalf("bad cursor err = %v", err)
	}
	cells, err := src.Grid(ctx, dep.ID, uxrecord.GridIO, from, to)
	if err != nil || len(cells) != 1 || cells[0].N != 3 {
		t.Fatalf("io grid = %+v, %v", cells, err)
	}
	total, withUsage, withCache, err := src.Coverage(ctx, 0, from, to)
	if err != nil || total != 3 || withUsage != 3 || withCache != 3 {
		t.Fatalf("coverage = %d %d %d, %v", total, withUsage, withCache, err)
	}

	// Lookup finds the trace across deployments and a request by id; a
	// window the store refuses maps to the monitor's bad-query error.
	found, err := src.Lookup(ctx, monitorapi.LookupQuery{TraceID: trace, FromMS: from, ToMS: to})
	if err != nil || len(found) != 3 || found[0].ID != "r0" || found[0].Trace.Trace != trace {
		t.Fatalf("lookup = %+v, %v", found, err)
	}
	found, err = src.Lookup(ctx, monitorapi.LookupQuery{RequestID: "r2", FromMS: from, ToMS: to})
	if err != nil || len(found) != 1 || found[0].ID != "r2" {
		t.Fatalf("lookup by id = %+v, %v", found, err)
	}
	if _, err := src.Lookup(ctx, monitorapi.LookupQuery{RequestID: "r2"}); !errors.Is(err, monitorapi.ErrBadHistoryQuery) {
		t.Fatalf("lookup without a window err = %v", err)
	}
	if got := src.DeploymentNames([]uint{dep.ID, dep.ID + 100}); len(got) != 1 || got[dep.ID] != dep.ServedName {
		t.Fatalf("deployment names = %v", got)
	}

	names := src.KeyNames()
	if names[key.ID] != "chat-app" {
		t.Fatalf("names = %v", names)
	}
	// Names are cached for the TTL: a new key shows up only after it.
	if _, err := st.CreateAPIKey("batch", 1); err != nil {
		t.Fatal(err)
	}
	if len(src.KeyNames()) != len(names) {
		t.Fatal("names refreshed before the TTL")
	}
	src.namesAt = time.Time{}
	if len(src.KeyNames()) != len(names)+1 {
		t.Fatal("names not refreshed after the TTL")
	}
}

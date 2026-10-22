package uxstore

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func traceOf(t *testing.T, hex string) tracecontext.TraceID {
	t.Helper()
	id, ok := tracecontext.ParseTraceID(hex)
	if !ok {
		t.Fatalf("bad trace id %q", hex)
	}
	return id
}

func tracedReq(id string, dep uint, ts int64, trace tracecontext.TraceID) uxrecord.Request {
	r := okReq(id, dep, ts)
	r.Trace.Trace = trace
	return r
}

// A trace is found across deployments and days, newest first, and its
// requests come back with the trace id; a request id finds its one request.
func TestLookupFindsATraceAcrossDeploymentsAndDays(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	agent := traceOf(t, "4bf92f3577b34da6a3ce929d0e0e4736")
	other := traceOf(t, "0af7651916cd43dd8448eb211c80319c")
	midnight := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for _, r := range []uxrecord.Request{
		tracedReq("a1", 1, ms(midnight.Add(-2*time.Minute)), agent),
		tracedReq("a2", 2, ms(midnight.Add(-time.Minute)), agent),
		tracedReq("b1", 1, ms(midnight.Add(-time.Minute)), other),
		tracedReq("a3", 3, ms(midnight.Add(time.Minute)), agent),
		okReq("plain", 1, ms(midnight)),
	} {
		s.Observe(r)
	}
	mustSync(t, s, false)

	from, to := ms(midnight.Add(-time.Hour)), ms(midnight.Add(time.Hour))
	got, err := s.Lookup(context.Background(), LookupQuery{TraceID: agent, FromMS: from, ToMS: to})
	if err != nil {
		t.Fatal(err)
	}
	if ids := requestIDs(got); ids != "a3,a2,a1" {
		t.Fatalf("trace lookup = %s", ids)
	}
	for _, r := range got {
		if r.Trace.Trace != agent || r.DeploymentID == 0 || r.Status != 200 {
			t.Fatalf("record = %+v", r)
		}
	}
	got, err = s.Lookup(context.Background(), LookupQuery{TraceID: agent, FromMS: from, ToMS: to, Limit: 2})
	if err != nil || requestIDs(got) != "a3,a2" {
		t.Fatalf("limited lookup = %s, %v", requestIDs(got), err)
	}
	got, err = s.Lookup(context.Background(), LookupQuery{RequestID: " b1 ", FromMS: from, ToMS: to})
	if err != nil || requestIDs(got) != "b1" || got[0].Trace.Trace != other {
		t.Fatalf("request id lookup = %+v, %v", got, err)
	}
	// The window bounds the search.
	got, err = s.Lookup(context.Background(), LookupQuery{TraceID: agent, FromMS: ms(midnight), ToMS: to})
	if err != nil || requestIDs(got) != "a3" {
		t.Fatalf("narrow lookup = %s, %v", requestIDs(got), err)
	}
	// And the history page carries the trace id as well.
	page := allRequests(t, s, RequestQuery{DeploymentID: 2})
	if len(page) != 1 || page[0].Trace.Trace != agent {
		t.Fatalf("history = %+v", page)
	}
	plain := allRequests(t, s, RequestQuery{DeploymentID: 1, FromMS: ms(midnight), ToMS: ms(midnight)})
	if len(plain) != 1 || !plain[0].Trace.Trace.IsZero() {
		t.Fatalf("a request without a trace = %+v", plain)
	}
}

func requestIDs(rs []uxrecord.Request) string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return strings.Join(ids, ",")
}

func TestLookupRefusesAQueryWithoutOneIDOrAWindow(t *testing.T) {
	s, _ := openTest(t, testOpts(newClock(day0)))
	trace := traceOf(t, "4bf92f3577b34da6a3ce929d0e0e4736")
	from := ms(day0)
	for name, q := range map[string]LookupQuery{
		"no id":           {FromMS: from, ToMS: from + 1},
		"both ids":        {TraceID: trace, RequestID: "x", FromMS: from, ToMS: from + 1},
		"blank id":        {RequestID: "  ", FromMS: from, ToMS: from + 1},
		"no window":       {TraceID: trace},
		"reversed":        {TraceID: trace, FromMS: from, ToMS: from - 1},
		"window too wide": {TraceID: trace, FromMS: from, ToMS: from + uxrecord.MaxLookupWindow.Milliseconds() + 1},
	} {
		if _, err := s.Lookup(context.Background(), q); !errors.Is(err, ErrBadQuery) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if got, err := s.Lookup(context.Background(), LookupQuery{TraceID: trace, FromMS: from, ToMS: from + uxrecord.MaxLookupWindow.Milliseconds()}); err != nil || len(got) != 0 {
		t.Fatalf("a full window on an empty store = %v, %v", got, err)
	}
}

// Over the row budget, a normal request may have no row; the lookup cannot
// find it, while the counts still have it.
func TestLookupMissesARequestTheLogSampledAway(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1
	s, _ := openTest(t, opts)
	trace := traceOf(t, "4bf92f3577b34da6a3ce929d0e0e4736")
	s.Observe(okReq("kept", 1, ms(day0)))
	s.Observe(tracedReq("sampled-away", 1, ms(day0), trace))
	mustSync(t, s, false)
	got, err := s.Lookup(context.Background(), LookupQuery{TraceID: trace, FromMS: ms(day0) - 1000, ToMS: ms(day0) + 1000})
	if err != nil || len(got) != 0 {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if st := s.Stats(); st.SampledAway != 1 {
		t.Fatalf("stats = %+v", st)
	}
	cov, err := s.Coverage(context.Background(), 1, ms(day0), ms(day0))
	if err != nil || cov.Total != 2 {
		t.Fatalf("coverage = %+v, %v", cov, err)
	}
}

// The lookup walks the deployment index of the request table; it never
// scans the table.
func TestLookupUsesTheDeploymentIndex(t *testing.T) {
	s, _ := openTest(t, testOpts(newClock(day0)))
	s.Observe(okReq("r", 1, ms(day0)))
	mustSync(t, s, false)
	day := dayOf(ms(day0))
	for _, match := range []string{"r.trace_id = ?", "r.id = ?"} {
		rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+lookupSQL(day, match), 0, 1, 0, 1, "x", 10)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "USING INDEX "+reqPrefix+day+"_dep (deployment_id=? AND ts>? AND ts<?)") {
			t.Fatalf("%s: plan = %s", match, joined)
		}
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN r") {
				t.Fatalf("%s: the request table is scanned: %s", match, joined)
			}
		}
	}
}

// A day written before trace_id existed gets the column when the store
// opens, its rows read with no trace; the schema version does not move, and
// the shape an older release writes (its column list) still inserts.
func TestOpenAddsTheNewColumnsToOlderDaysAndKeepsTheirShapeWritable(t *testing.T) {
	path := t.TempDir() + "/ux.db"
	day := dayOf(ms(day0))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append(olderShapeDDL(day), `PRAGMA user_version = 2`)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := insertLikeAnOlderRelease(db, day, "before", ms(day0)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for round := 0; round < 2; round++ { // the second open finds nothing to add
		s := openAt(t, path, testOpts(newClock(day0)))
		for _, added := range addedColumns {
			got := columns(t, s.writer, added.prefix+day)
			for _, col := range added.cols {
				if !slices.Contains(got, col.name) {
					t.Fatalf("round %d: %s lacks %s: %v", round, added.prefix+day, col.name, got)
				}
			}
		}
		if err := insertLikeAnOlderRelease(s.writer, day, "rolled-back-"+itoa(int64(round)), ms(day0)+1); err != nil {
			t.Fatalf("round %d: an older release's insert fails: %v", round, err)
		}
		if _, err := s.writer.Exec(`INSERT INTO `+latPrefix+day+` (bucket_ms, deployment_id, requests, e2e_ms, gateway_ms, preprocess_ms, queue_ms, prefill_ms,
			waiting_ms, thinking_ms, answering_ms, tail_ms, prefill_speed_tokens, prefill_speed_ms, thinking_speed_tokens,
			thinking_speed_ms, answering_speed_tokens, answering_speed_ms, decode_speed_tokens, decode_speed_ms)
			VALUES (?,1,1,10,1,0,0,0,9,0,0,0,0,0,0,0,0,0,0,0)
			ON CONFLICT (deployment_id, bucket_ms) DO UPDATE SET requests = requests + 1`, floorBucket(ms(day0))+int64(round)); err != nil {
			t.Fatalf("round %d: an older release's latency upsert fails: %v", round, err)
		}
		rows := allRequests(t, s, RequestQuery{DeploymentID: 1})
		if len(rows) != round+2 {
			t.Fatalf("round %d rows = %d", round, len(rows))
		}
		for _, r := range rows {
			if !r.Trace.Trace.IsZero() || r.PD != nil {
				t.Fatalf("an old row has a trace or a PD stage: %+v", r)
			}
		}
		s.Close()
		if v := userVersion(t, path); v != schemaVersion {
			t.Fatalf("user_version = %d", v)
		}
	}
}

// olderShapeDDL is a day's tables as the release before the added columns
// created them.
func olderShapeDDL(day string) []string {
	stmts := partitionDDL(day)
	for _, added := range addedColumns {
		for _, col := range added.cols {
			stmts = append(stmts, `ALTER TABLE `+added.prefix+day+` DROP COLUMN `+col.name)
		}
	}
	return stmts
}

// insertLikeAnOlderRelease writes a row with the column list of the release
// before trace_id.
func insertLikeAnOlderRelease(db *sql.DB, day, id string, ts int64) error {
	_, err := db.Exec(`INSERT INTO `+reqPrefix+day+` (id, ts, deployment_id, instance_uuid, api_key_id, user_id, source, path, stream,
		body_bytes, images, dispatch_ms, first_byte_ms, first_think_ms, first_answer_ms, last_chunk_ms, end_ms,
		events, think_events, max_gap_ms, status, outcome, finish_reason, usage, prompt_tokens, completion_tokens,
		cached_tokens, reasoning_tokens, engine_queue_ms, engine_prefill_ms, engine_decode_ms, weight)
		VALUES (?,?,1,'inst',0,0,0,'/v1/chat/completions',1,0,0,0,0,-1,0,0,10,1,0,0,200,0,'stop',0,0,0,-1,-1,-1,-1,-1,1)`, id, ts)
	return err
}

// A column add that fails is reported, so Open fails rather than run with a
// table its inserts do not fit.
func TestCompleteRequestTableReportsAFailedAdd(t *testing.T) {
	path := t.TempDir() + "/ux.db"
	day := dayOf(ms(day0))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range olderShapeDDL(day) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := completePartition(ro, day); err == nil || !strings.Contains(err.Error(), "trace_id") {
		t.Fatalf("err = %v", err)
	}
	// A day without a request table has nothing to complete.
	if err := completePartition(ro, "20200101"); err != nil {
		t.Fatalf("missing table: %v", err)
	}
}

// A PD request's stage round-trips through the request log; a request
// without one reads back without one; the latency sums keep the stage.
func TestAPDStageIsStoredAndSummed(t *testing.T) {
	s, _ := openTest(t, testOpts(newClock(day0)))
	pd := okReq("pd", 1, ms(day0))
	pd.DispatchMS, pd.FirstAnswerMS = 816, 900
	pd.PD = &uxrecord.PDStage{PrefillInstance: "p-0", PrefillMS: 795, PrefillQueueMS: 2, PrefillComputeMS: 640}
	fell := okReq("fell", 1, ms(day0)+1)
	// A stage reported longer than the dispatch (1 ms) counts up to it.
	fell.PD = &uxrecord.PDStage{PrefillMS: 3, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: uxrecord.Unknown, Fallback: "prefill_5xx"}
	for _, r := range []uxrecord.Request{pd, fell, okReq("plain", 1, ms(day0)+2)} {
		s.Observe(r)
	}
	mustSync(t, s, false)
	got := map[string]*uxrecord.PDStage{}
	for _, r := range allRequests(t, s, RequestQuery{DeploymentID: 1}) {
		got[r.ID] = r.PD
	}
	if p := got["pd"]; p == nil || *p != (uxrecord.PDStage{PrefillInstance: "p-0", PrefillMS: 795, PrefillQueueMS: 2, PrefillComputeMS: 640}) {
		t.Fatalf("pd = %+v", p)
	}
	if p := got["fell"]; p == nil || p.Fallback != "prefill_5xx" || p.PrefillInstance != "" || p.PrefillQueueMS != uxrecord.Unknown {
		t.Fatalf("fell back = %+v", p)
	}
	if got["plain"] != nil {
		t.Fatalf("plain = %+v", got["plain"])
	}
	sums, err := s.Latency(context.Background(), 1, ms(day0)-1000, ms(day0)+1000)
	if err != nil || sums.SegmentMS[uxrecord.SegmentPDPrefill] != 795+1 || sums.SegmentMS[uxrecord.SegmentGateway] != 21+0+1 {
		t.Fatalf("sums = %+v, %v", sums.SegmentMS, err)
	}
}

package usagereport

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoundKeepsLargestAndFoldsTheRest(t *testing.T) {
	var in []Count
	total := 0
	for i := 1; i <= 40; i++ {
		in = append(in, Count{fmt.Sprintf("m-%02d", i), i})
		total += i
	}
	in = append(in, Count{"", 2}, Count{"  ", 1}, Count{strings.Repeat("x", 500), 1}, Count{"zero", 0}, Count{"neg", -3})
	out := Bound(in, MaxGroups)
	if len(out) != MaxGroups || out[0] != (Count{"m-40", 40}) || out[MaxGroups-1].Name != Other {
		t.Fatalf("bound = %v", out)
	}
	sum := 0
	for _, c := range out {
		sum += c.N
		if len(c.Name) > MaxName {
			t.Fatalf("name %q longer than %d", c.Name, MaxName)
		}
	}
	if sum != total+4 {
		t.Fatalf("sum %d, want %d: nothing positive may be lost", sum, total+4)
	}
	if got := Bound([]Count{{"", 2}, {" ", 1}}, 4); len(got) != 1 || got[0] != (Count{Unknown, 3}) {
		t.Fatalf("empty names = %v, want one unknown group", got)
	}
}

func TestCountsWireFormatIsAnObject(t *testing.T) {
	raw, err := json.Marshal(Counts{{"NVIDIA H20", 8}, {"a\"b", 1}})
	if err != nil || string(raw) != `{"NVIDIA H20":8,"a\"b":1}` {
		t.Fatalf("marshal = %s, %v", raw, err)
	}
	var back Counts
	if err := json.Unmarshal(raw, &back); err != nil || len(back) != 2 || back[0] != (Count{"NVIDIA H20", 8}) {
		t.Fatalf("round trip = %v, %v", back, err)
	}
	empty, _ := json.Marshal(Counts(nil))
	if string(empty) != `{}` {
		t.Fatalf("empty = %s, want {}", empty)
	}
}

func TestCountersDrainRestoreAndCap(t *testing.T) {
	var nilCounters *Counters
	nilCounters.Inc(KindRoute, "nodes") // must not panic
	if nilCounters.Drain() != nil {
		t.Fatal("nil counters drained something")
	}

	c := &Counters{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Inc(KindRoute, "nodes") }()
	}
	wg.Wait()
	c.Inc(KindAPIError, "NODE_NOT_FOUND nodes")
	got := c.Drain()
	if got[KindRoute][0] != (Count{"nodes", 50}) || got[KindAPIError][0].N != 1 {
		t.Fatalf("drain = %v", got)
	}
	if again := c.Drain(); len(again) != 0 {
		t.Fatalf("second drain = %v, want empty", again)
	}
	c.Inc(KindRoute, "nodes")
	c.Restore(got)
	if back := c.Drain(); back[KindRoute][0] != (Count{"nodes", 51}) {
		t.Fatalf("restored = %v, want the undelivered counts added to the new ones", back)
	}

	for i := 0; i < maxKeysPerKind+10; i++ {
		c.Inc(KindPanic, strconv.Itoa(i))
	}
	panics := c.Drain()[KindPanic]
	if len(panics) != maxKeysPerKind+1 || panics[0] != (Count{Other, 10}) {
		t.Fatalf("%d keys, first %v; want the cap plus one other group of 10", len(panics), panics[0])
	}
}

func TestNextReportsOnlyTheLatestWholeDay(t *testing.T) {
	now := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	off := spread("abc")
	if off < 0 || off >= dueSpread {
		t.Fatalf("spread %v out of range", off)
	}
	if spread("abc") != off {
		t.Fatal("spread is not stable for one installation")
	}
	for _, tc := range []struct {
		last    string
		wantDay time.Time
	}{
		{"", yesterday},
		{"2026-10-01", yesterday}, // a week down: only yesterday, no backlog
		{"2026-10-21", yesterday.AddDate(0, 0, 1)},
		{"garbage", yesterday},
	} {
		day, due := Next("abc", tc.last, now)
		if !day.Equal(tc.wantDay) {
			t.Errorf("last %q: day %v, want %v", tc.last, day, tc.wantDay)
		}
		if want := tc.wantDay.AddDate(0, 0, 1).Add(30*time.Minute + off); !due.Equal(want) {
			t.Errorf("last %q: due %v, want %v", tc.last, due, want)
		}
	}
	since, until := Window(time.Date(2026, 10, 21, 13, 0, 0, 0, time.FixedZone("x", 8*3600)))
	if !since.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) || until.Sub(since) != 24*time.Hour {
		t.Fatalf("window = %v..%v", since, until)
	}
}

// slsFixture answers like PutWebtracking: it checks the API version, the raw
// size header and the deflate body, and refuses with SLS's error shape.
type slsFixture struct {
	t       *testing.T
	delay   time.Duration
	status  int
	code    string
	mu      sync.Mutex
	bodies  []slsLogGroup
	headers []http.Header
}

func (f *slsFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, r.Header.Clone())
	time.Sleep(f.delay)
	if r.Method == http.MethodGet {
		// A GET without parameters writes nothing (what the probe sends).
		writeSLSError(w, http.StatusBadRequest, "ParameterInvalid")
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/logstores/reports/track") {
		writeSLSError(w, http.StatusNotFound, "LogStoreNotExist")
		return
	}
	if r.Header.Get("x-log-apiversion") != "0.6.0" {
		writeSLSError(w, http.StatusBadRequest, "InvalidAPIVersion")
		return
	}
	raw := r.Body.(io.Reader)
	if r.Header.Get("x-log-compresstype") == "deflate" {
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			writeSLSError(w, http.StatusBadRequest, "PostBodyUncompressError")
			return
		}
		raw = zr
	}
	body, err := io.ReadAll(raw)
	if err != nil {
		writeSLSError(w, http.StatusBadRequest, "PostBodyUncompressError")
		return
	}
	if size, _ := strconv.Atoi(r.Header.Get("x-log-bodyrawsize")); size != len(body) {
		writeSLSError(w, http.StatusBadRequest, "PostBodyInvalid")
		return
	}
	// The service takes these four keys and nothing else (checked against
	// the real logstore on 2026-10-22).
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writeSLSError(w, http.StatusBadRequest, "PostBodyInvalid")
		return
	}
	for k := range keys {
		switch k {
		case "__topic__", "__source__", "__logs__", "__tags__":
		default:
			writeSLSError(w, http.StatusBadRequest, "PostBodyInvalid")
			return
		}
	}
	var group slsLogGroup
	if err := json.Unmarshal(body, &group); err != nil || group.Source == "" || len(group.Logs) == 0 {
		writeSLSError(w, http.StatusBadRequest, "PostBodyInvalid")
		return
	}
	f.bodies = append(f.bodies, group)
	if f.status != 0 {
		writeSLSError(w, f.status, f.code)
		return
	}
	// Success is a 200 with an empty body.
	w.WriteHeader(http.StatusOK)
}

func writeSLSError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errorCode":%q,"errorMessage":"fixture"}`, code)
}

type collectorFixture struct {
	mu      sync.Mutex
	reports [][]byte
	status  int
	delay   time.Duration
}

func (f *collectorFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	time.Sleep(f.delay)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Content-Encoding") != "gzip" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(zr)
	f.reports = append(f.reports, body)
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func testReport() *Report {
	return &Report{Schema: Schema, InstallID: "0123456789abcdef", Day: "2026-10-21",
		Server: Server{Version: "v1.2.3", Channel: "nightly", OS: "linux", Arch: "amd64"}}
}

func endpoints(sls, collector *httptest.Server) []Endpoint {
	return []Endpoint{
		{Kind: EndpointSLS, URL: sls.URL + "/logstores/reports/track"},
		{Kind: EndpointCollector, URL: collector.URL + "/v1/report"},
	}
}

func TestSendGoesToSLSOnlyWhenItAccepts(t *testing.T) {
	sls, collector := &slsFixture{t: t}, &collectorFixture{}
	slsSrv, colSrv := httptest.NewServer(sls), httptest.NewServer(collector)
	defer slsSrv.Close()
	defer colSrv.Close()
	s := &Sender{Endpoints: endpoints(slsSrv, colSrv)}
	got, attempts, err := s.Send(context.Background(), testReport())
	if err != nil || got != EndpointSLS || len(attempts) != 1 {
		t.Fatalf("send = %q %v %v", got, attempts, err)
	}
	if len(collector.reports) != 0 {
		t.Fatal("the collector was asked although SLS took the report")
	}
	log := sls.bodies[0].Logs[0]
	if log["install_id"] != "0123456789abcdef" || log["day"] != "2026-10-21" || log["schema"] != strconv.Itoa(Schema) || log["version"] != "v1.2.3" {
		t.Fatalf("log keys = %v", log)
	}
	var back Report
	if err := json.Unmarshal([]byte(log["report"]), &back); err != nil || back.InstallID != "0123456789abcdef" {
		t.Fatalf("report field = %s, %v", log["report"], err)
	}
	if sls.bodies[0].Topic != "usage" || sls.bodies[0].Source != "gpuworks-server" {
		t.Fatalf("group = %+v", sls.bodies[0])
	}
}

func TestSendFallsBackToTheCollector(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{{"logstore missing", 404, "LogStoreNotExist"}, {"server error", 500, "InternalServerError"}, {"forbidden", 403, "Unauthorized"}} {
		t.Run(tc.name, func(t *testing.T) {
			sls, collector := &slsFixture{t: t, status: tc.status, code: tc.code}, &collectorFixture{}
			slsSrv, colSrv := httptest.NewServer(sls), httptest.NewServer(collector)
			defer slsSrv.Close()
			defer colSrv.Close()
			got, attempts, err := (&Sender{Endpoints: endpoints(slsSrv, colSrv)}).Send(context.Background(), testReport())
			if err != nil || got != EndpointCollector || len(attempts) != 2 {
				t.Fatalf("send = %q %v %v", got, attempts, err)
			}
			var endpointErr *EndpointError
			if !errors.As(attempts[0].Err, &endpointErr) || endpointErr.Status != tc.status || endpointErr.Code != tc.code {
				t.Fatalf("sls attempt = %v", attempts[0].Err)
			}
			var back Report
			if err := json.Unmarshal(collector.reports[0], &back); err != nil || back.Day != "2026-10-21" {
				t.Fatalf("collector got %s, %v", collector.reports[0], err)
			}
		})
	}
}

func TestSendFailsWhenEveryEndpointFails(t *testing.T) {
	sls, collector := &slsFixture{t: t, status: 500, code: "InternalServerError"}, &collectorFixture{status: 503}
	slsSrv, colSrv := httptest.NewServer(sls), httptest.NewServer(collector)
	defer slsSrv.Close()
	colSrv.Close() // unreachable
	_, attempts, err := (&Sender{Endpoints: endpoints(slsSrv, colSrv)}).Send(context.Background(), testReport())
	if err == nil || len(attempts) != 2 || attempts[1].Err == nil {
		t.Fatalf("send = %v %v, want both attempts failed", attempts, err)
	}
	if len(sls.bodies) != 1 {
		t.Fatalf("sls asked %d times, want once (no retries)", len(sls.bodies))
	}
}

func TestSendStopsWhenTheCallerGivesUp(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	collector := &collectorFixture{}
	colSrv := httptest.NewServer(collector)
	defer colSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, attempts, err := (&Sender{Endpoints: endpoints(slow, colSrv)}).Send(ctx, testReport())
	if err == nil || len(attempts) != 1 || len(collector.reports) != 0 {
		t.Fatalf("send = %v %v, collector %d; want to stop after the cancelled attempt", attempts, err, len(collector.reports))
	}
}

func TestVersionCutsKeepOnlyNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"6.8.0-138-generic": "6.8", "12.4.1": "12.4", "v0.9.1": "0.9", "8.0.RC2": "8.0", "5": "5",
		"4.18.0-372.el8.secret-host": "4.18", "": "", "rolling": "", "550abc": "550",
	} {
		if got := MajorMinor(in); got != want {
			t.Errorf("MajorMinor(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"550.54.15": "550", "v25.0": "25", "x": ""} {
		if got := Major(in); got != want {
			t.Errorf("Major(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuckets(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "<30s", 90 * time.Second: "30s-2m", 5 * time.Minute: "2m-10m",
		20 * time.Minute: "10m-30m", time.Hour: "30m-2h", 3 * time.Hour: ">2h"} {
		if got := DurationBucket(d); got != want {
			t.Errorf("DurationBucket(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{0: Unknown, 1: "<1G", 16 << 30: "10G-100G", 200 << 30: "100G-500G", 1 << 40: ">500G"} {
		if got := SizeBucket(n); got != want {
			t.Errorf("SizeBucket(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestEncodeBoundsAndTruncates(t *testing.T) {
	r := testReport()
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("GET /route-%04d/%s", i, strings.Repeat("x", 40))
		r.Usage.Routes = append(r.Usage.Routes, Count{name, i + 1})
		r.Errors.Events = append(r.Errors.Events, Count{name, 1})
	}
	raw, err := Encode(r)
	if err != nil || len(raw) > MaxEncoded {
		t.Fatalf("encoded %d bytes, %v", len(raw), err)
	}
	if len(r.Usage.Routes) != maxRouteGroups || len(r.Errors.Events) != maxEventGroups || r.Truncated {
		t.Fatalf("routes %d events %d truncated %v", len(r.Usage.Routes), len(r.Errors.Events), r.Truncated)
	}
	var back map[string]any
	raw, _ = Encode(testReport())
	if err := json.Unmarshal(raw, &back); err != nil || back["truncated"] != nil {
		t.Fatalf("small report = %s", raw)
	}
	for _, key := range []string{"environment", "usage", "tasks", "errors"} {
		if _, ok := back[key]; !ok {
			t.Fatalf("report lacks %q: %s", key, raw)
		}
	}
}

func TestPanicSiteNamesThePlatformFunction(t *testing.T) {
	var site string
	func() {
		defer func() {
			if recover() != nil {
				site = PanicSite()
			}
		}()
		panicHere()
	}()
	if site != "server/usagereport.panicHere" {
		t.Fatalf("site = %q", site)
	}
	if strings.Contains(site, "/home/") || strings.Contains(site, ".go") {
		t.Fatalf("site %q carries a path", site)
	}
}

//go:noinline
func panicHere() { panic("boom") }

func TestCountersPeekLeavesTheCounts(t *testing.T) {
	c := &Counters{}
	c.Inc(KindLogin, "ok")
	if p := c.Peek(); p[KindLogin][0] != (Count{"ok", 1}) {
		t.Fatalf("peek = %v", p)
	}
	if d := c.Drain(); d[KindLogin][0] != (Count{"ok", 1}) {
		t.Fatalf("drain after peek = %v", d)
	}
}

func sampleFleetReport() *FleetReport {
	return &FleetReport{
		InstallID: "0123456789abcdef", ServerVersion: "v0.9.1", Nodes: 5, OnlineNodes: 4,
		AgentVersions: []Count{{"v0.9.1", 4}, {"v0.9.0", 1}},
		GPUModels:     []Count{{"NVIDIA A100-SXM4-80GB", 16}, {"NVIDIA H20", 8}},
	}
}

// The report is exactly these parameters and nothing else; a model name with
// separators in it stays one value.
func TestFleetReportQueryWireFormat(t *testing.T) {
	report := sampleFleetReport()
	report.GPUModels = append(report.GPUModels, Count{"Weird, name&x=1:2", 3})
	q := report.Query()
	want := url.Values{
		"id": {"0123456789abcdef"}, "v": {"v0.9.1"}, "nodes": {"5"}, "online": {"4"},
		"agent": {"v0.9.1:4", "v0.9.0:1"},
		"gpu":   {"NVIDIA A100-SXM4-80GB:16", "NVIDIA H20:8", "Weird, name&x=1:2:3"},
	}
	if fmt.Sprint(q) != fmt.Sprint(want) {
		t.Fatalf("query = %v\nwant    %v", q, want)
	}
	parsed, err := url.ParseQuery(q.Encode())
	if err != nil || fmt.Sprint(parsed) != fmt.Sprint(want) {
		t.Fatalf("round trip = %v, %v", parsed, err)
	}
}

// However varied the fleet, the request stays short: the biggest groups are
// kept and the tail is summed into "other".
func TestFleetReportQueryBoundsGroups(t *testing.T) {
	report := &FleetReport{}
	total := 0
	for i := 1; i <= 40; i++ {
		report.GPUModels = append(report.GPUModels, Count{fmt.Sprintf("model-%02d", i), i})
		total += i
	}
	report.GPUModels = append(report.GPUModels, Count{"", 2}, Count{strings.Repeat("x", 500), 1}, Count{"gone", 0})
	groups := report.Query()["gpu"]
	if len(groups) != MaxGroups {
		t.Fatalf("%d groups, want %d: %v", len(groups), MaxGroups, groups)
	}
	if groups[0] != "model-40:40" {
		t.Fatalf("largest first, got %q", groups[0])
	}
	sum := 0
	for _, g := range groups {
		n := 0
		fmt.Sscanf(g[strings.LastIndex(g, ":")+1:], "%d", &n)
		sum += n
		if len(g) > MaxName+8 {
			t.Fatalf("group %q is too long", g)
		}
	}
	if sum != total+3 {
		t.Fatalf("groups add up to %d cards, want %d (nothing may be lost, a zero count adds nothing)", sum, total+3)
	}
	if last := groups[len(groups)-1]; !strings.HasPrefix(last, Other+":") {
		t.Fatalf("tail is %q, want other", last)
	}
}

func probeMS(r *Report, kind string) int {
	for _, c := range r.Delivery.ProbeMS {
		if c.Name == kind {
			return c.N
		}
	}
	return -2
}

// The report goes where the probe answered first; the primary keeps it on
// a near tie, and an endpoint that does not answer is tried last.
func TestSendGoesToTheFasterEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name               string
		slsDelay, colDelay time.Duration
		slsDown            bool
		want               string
	}{
		{name: "near tie keeps the primary", want: EndpointSLS},
		{name: "collector clearly faster", slsDelay: 300 * time.Millisecond, want: EndpointCollector},
		{name: "collector a little faster", slsDelay: 20 * time.Millisecond, want: EndpointSLS},
		{name: "sls clearly faster", colDelay: 300 * time.Millisecond, want: EndpointSLS},
		{name: "sls unreachable", slsDown: true, want: EndpointCollector},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sls, collector := &slsFixture{t: t, delay: tc.slsDelay}, &collectorFixture{delay: tc.colDelay}
			slsSrv, colSrv := httptest.NewServer(sls), httptest.NewServer(collector)
			defer slsSrv.Close()
			defer colSrv.Close()
			eps := endpoints(slsSrv, colSrv)
			if tc.slsDown {
				slsSrv.Close()
			}
			r := testReport()
			got, attempts, err := (&Sender{Endpoints: eps}).Send(context.Background(), r)
			if err != nil || got != tc.want || len(attempts) != 1 {
				t.Fatalf("send = %q %v %v", got, attempts, err)
			}
			if r.Delivery.Endpoint != tc.want {
				t.Fatalf("delivery = %+v", r.Delivery)
			}
			if tc.slsDown && probeMS(r, EndpointSLS) != unreachable {
				t.Fatalf("probe of a closed endpoint = %v", r.Delivery.ProbeMS)
			}
			if tc.slsDelay >= 300*time.Millisecond && probeMS(r, EndpointSLS) < 300 {
				t.Fatalf("probe = %v", r.Delivery.ProbeMS)
			}
			// The other endpoint got the probe only, never the report.
			if tc.want == EndpointSLS && len(collector.reports) != 0 {
				t.Fatal("the collector got the report too")
			}
			if tc.want == EndpointCollector && len(sls.bodies) != 0 {
				t.Fatal("sls got the report too")
			}
			// The report as delivered says how it was delivered.
			var sent Report
			if tc.want == EndpointCollector {
				_ = json.Unmarshal(collector.reports[0], &sent)
			} else {
				_ = json.Unmarshal([]byte(sls.bodies[0].Logs[0]["report"]), &sent)
			}
			if sent.Delivery.Endpoint != tc.want || len(sent.Delivery.ProbeMS) != 2 {
				t.Fatalf("delivered delivery = %+v", sent.Delivery)
			}
		})
	}
}

func TestProbeTreatsServerErrorsAsUnreachable(t *testing.T) {
	sls := &slsFixture{t: t}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	slsSrv := httptest.NewServer(sls)
	defer slsSrv.Close()
	s := &Sender{}
	order, ms := s.probe(context.Background(), []Endpoint{{Kind: EndpointCollector, URL: down.URL}, {Kind: EndpointSLS, URL: slsSrv.URL}})
	if order[0].Kind != EndpointSLS || ms[0] != unreachable || ms[1] < 0 {
		t.Fatalf("order %v ms %v", order, ms)
	}
}

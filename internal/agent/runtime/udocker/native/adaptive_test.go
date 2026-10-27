package native

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
	"gpuworks.ai/gpuworks/internal/testenv/fakeregistry"
)

// These tests pull blobs from a fake registry that behaves like a throttled,
// connection-limited private registry in front of a blob CDN (see
// fakeregistry), through the per-host connection budget the engine uses, and
// assert how many connections the pull really opens and what it does when the
// registry pushes back.

const miB = 1 << 20

type regFixture struct {
	reg    *fakeregistry.Registry
	hosts  *connbudget.Hosts
	client *Client
	ref    Ref
	descs  []Descriptor
	data   map[string][]byte

	logMu sync.Mutex
	logs  []string
	waits waitLog
}

// newRegFixture starts a registry with blobs of the given sizes, a budget of
// ceiling connections per host, and spans of 1 MiB so that a small blob is
// cut into many.
func newRegFixture(t *testing.T, opts fakeregistry.Options, ceiling int, sizes ...int) *regFixture {
	t.Helper()
	old := rangeSpanTarget
	rangeSpanTarget = miB
	t.Cleanup(func() { rangeSpanTarget = old })

	f := &regFixture{reg: fakeregistry.New(t, opts), hosts: connbudget.NewHosts(ceiling), data: map[string][]byte{}}
	for i, size := range sizes {
		data := fakeregistry.Pattern(i+1, size)
		digest := f.reg.AddBlob(data)
		f.data[digest] = data
		f.descs = append(f.descs, Descriptor{Digest: digest, Size: int64(size)})
	}
	f.client = NewClient()
	transport := f.reg.Client().Transport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	transport.ResponseHeaderTimeout = time.Second // production uses 60s; a stalled answer must not hang a test
	f.client.HTTP = &http.Client{Transport: transport}
	f.ref = ParseRef(f.reg.Host() + "/repo/blob:latest")
	t.Cleanup(f.closeIdle)
	return f
}

func (f *regFixture) logged(substr string) int {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	n := 0
	for _, line := range f.logs {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// downloader returns a Downloader on the fixture's budget. adjust may change
// the session's configuration further.
func (f *regFixture) downloader(t *testing.T, adjust func(*connbudget.SessionConfig)) *Downloader {
	t.Helper()
	return &Downloader{
		Client: f.client, Dir: t.TempDir(), Workers: 3, RangeParts: 4, RangeMin: 1,
		Budget: f.hosts,
		Tune: func(cfg *connbudget.SessionConfig) {
			cfg.Window = 120 * time.Millisecond
			cfg.SlowFloor = 1 << 10 // far below anything these tests deliver
			cfg.Log = func(msg string, kv ...any) {
				f.logMu.Lock()
				defer f.logMu.Unlock()
				line := msg
				for i := 0; i+1 < len(kv); i += 2 {
					line += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
				}
				f.logs = append(f.logs, line)
			}
			cfg.Wait = func(ctx context.Context, d time.Duration) error {
				f.waits.add(d)
				return ctx.Err()
			}
			if adjust != nil {
				adjust(cfg)
			}
		},
	}
}

// pull downloads every blob and checks each arrived whole, then that nothing
// is left behind: every budget token returned, no connection held open by the
// registry, no goroutine left running.
func (f *regFixture) pull(t *testing.T, d *Downloader) (reported int64, err error) {
	t.Helper()
	before := runtime.NumGoroutine()
	var total atomic.Int64
	err = d.FetchAll(context.Background(), f.ref, f.descs, func(_ string, n int64) { total.Add(n) }, nil)
	f.assertQuiet(t, before)
	if err == nil {
		for _, desc := range f.descs {
			got, readErr := os.ReadFile(d.BlobPath(desc.Digest))
			if readErr != nil || string(got) != string(f.data[desc.Digest]) {
				t.Fatalf("blob %s is not what the registry holds (%v)", shortDigest(desc.Digest), readErr)
			}
		}
	}
	return total.Load(), err
}

// closeIdle drops the client's idle connections, including those of the copy
// of the transport that loopback (insecure) registries are spoken to with.
func (f *regFixture) closeIdle() {
	f.client.HTTP.CloseIdleConnections()
	f.client.mu.Lock()
	insecure := f.client.insecureHTTP
	f.client.mu.Unlock()
	if insecure != nil {
		insecure.CloseIdleConnections()
	}
}

func (f *regFixture) assertQuiet(t *testing.T, goroutinesBefore int) {
	t.Helper()
	if in := f.hosts.For(f.client.BudgetKey(context.Background(), f.ref)).InUse(); in != 0 {
		t.Fatalf("%d budget tokens were never returned", in)
	}
	f.closeIdle()
	deadline := time.Now().Add(5 * time.Second)
	for {
		open, goroutines := f.reg.OpenConns(), runtime.NumGoroutine()
		if open == 0 && goroutines <= goroutinesBefore+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the pull: %d connections still open at the registry, %d goroutines (%d before)", open, goroutines, goroutinesBefore)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With a per-connection speed limit, more connections are more speed: the pull
// starts at four and must climb well past them, up to but never past the
// ceiling.
func TestPullClimbsUnderAPerConnectionLimitButNeverPastTheCeiling(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 2 * miB}, 12, 24*miB)
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	stats := f.reg.Stats()
	if stats.PeakBusy <= startConnections {
		t.Fatalf("peak %d concurrent requests: the pull never climbed past its start of %d although each connection is capped", stats.PeakBusy, startConnections)
	}
	if stats.PeakBusy > 12 || stats.PeakConns > 12 {
		t.Fatalf("peak %d requests on %d connections, ceiling 12", stats.PeakBusy, stats.PeakConns)
	}
	if f.logged("connections changed") == 0 {
		t.Fatal("the climb was not logged")
	}
	if stats.Redirects == 0 {
		t.Fatal("the registry never redirected to its CDN: the fixture is not the shape under test")
	}
}

// A registry address that caps the whole address (not each connection) gains
// nothing from more connections: the climb tries one doubling, sees no gain and
// steps back instead of holding the larger number.
func TestPullStopsClimbingWhereTheAddressBandwidthIsAlreadyFull(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 2 * miB, AggregateRate: 8 * miB}, 16, 20*miB)
	d := f.downloader(t, func(cfg *connbudget.SessionConfig) { cfg.Window = 250 * time.Millisecond })
	if _, err := f.pull(t, d); err != nil {
		t.Fatal(err)
	}
	if peak := f.reg.Stats().PeakBusy; peak > 8 {
		t.Fatalf("peak %d concurrent requests: four connections already fill the address, the trial of eight was the most that was warranted", peak)
	}
	if f.logged("reverted: no gain") == 0 {
		t.Fatalf("the doubling that brought nothing was not stepped back: %v", f.logs)
	}
}

// The ceiling is a hard limit, also when asked for a single connection.
func TestPullNeverExceedsAConfiguredCeiling(t *testing.T) {
	for _, ceiling := range []int{1, 3} {
		f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 8 * miB}, ceiling, 6*miB)
		if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
			t.Fatalf("ceiling %d: %v", ceiling, err)
		}
		if stats := f.reg.Stats(); stats.PeakBusy > ceiling || stats.PeakConns > ceiling {
			t.Fatalf("ceiling %d: peak %d requests on %d connections", ceiling, stats.PeakBusy, stats.PeakConns)
		}
	}
}

// Two pulls from one registry share its budget: together they stay under the
// ceiling, and both finish.
func TestConcurrentPullsFromOneRegistryShareItsCeiling(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 4 * miB}, 4, 8*miB, 8*miB)
	first, second := f.downloader(t, nil), f.downloader(t, nil)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, d := range []*Downloader{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.FetchAll(context.Background(), f.ref, f.descs[i:i+1], nil, nil)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if stats := f.reg.Stats(); stats.PeakBusy > 4 || stats.PeakConns > 4 {
		t.Fatalf("two pulls opened %d requests on %d connections at once, ceiling 4", stats.PeakBusy, stats.PeakConns)
	}
	if in := f.hosts.For(f.client.BudgetKey(context.Background(), f.ref)).InUse(); in != 0 {
		t.Fatalf("%d tokens never returned", in)
	}
}

// A registry that allows three connections per client and answers the fourth
// with 429 and Retry-After: the pull backs off to what the registry accepts,
// waits what it was told, and is not answered 429 over and over.
func TestPullBacksOffAndHonoursRetryAfterOnAConnectionLimit429(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 4 * miB, MaxConns: 3, RetryAfter: 7}, 16, 12*miB)
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	stats := f.reg.Stats()
	if stats.Throttled == 0 {
		t.Fatal("the registry never limited the pull: the fixture is not the shape under test")
	}
	if stats.PeakBusy > 3 {
		t.Fatalf("%d concurrent requests against a limit of 3", stats.PeakBusy)
	}
	if stats.Throttled > 6 {
		t.Fatalf("%d 429s: the pull kept asking instead of backing off", stats.Throttled)
	}
	sawRetryAfter := false
	for _, d := range f.waits.all() {
		// What is left of the 7s by the time the request asks to wait.
		if d > 6*time.Second && d <= 7*time.Second {
			sawRetryAfter = true
		}
	}
	if !sawRetryAfter {
		t.Fatalf("waits %v never include the registry's Retry-After of 7s", f.waits.all())
	}
	if f.logged("backed off") == 0 {
		t.Fatalf("no backoff was logged: %v", f.logs)
	}
}

// A firewall that resets connections beyond a per-address limit says nothing
// useful: the pull backs off after repeated resets, and a span whose connection
// is reset while the others keep delivering is retried, not failed.
func TestPullSurvivesAPerAddressLimitThatResetsConnections(t *testing.T) {
	quickRecovery(t)
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 4 * miB, MaxConns: 3, ResetOverLimit: true}, 16, 12*miB)
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	stats := f.reg.Stats()
	if stats.Reset == 0 {
		t.Fatal("no connection was reset: the fixture is not the shape under test")
	}
	if stats.PeakBusy > 3 {
		t.Fatalf("%d concurrent requests against a limit of 3", stats.PeakBusy)
	}
	if f.logged("reason=connection resets") == 0 {
		t.Fatalf("repeated resets did not back the pull off: %v", f.logs)
	}
}

// A 429 from the CDN with a Retry-After is waited out by the pull, not retried
// at once, whatever the connection count is.
func TestPullWaitsOutARetryAfterBeforeReconnecting(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true}, 16, 4*miB)
	var limited atomic.Bool
	f.reg.SetFault(func(fakeregistry.Request) *fakeregistry.Fault {
		if limited.Swap(true) {
			return nil
		}
		return &fakeregistry.Fault{Status: http.StatusTooManyRequests, RetryAfter: "3"}
	})
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	got := f.waits.all()
	if len(got) == 0 {
		t.Fatal("the pull reconnected at once after a 429")
	}
	for _, d := range got {
		if d > 3*time.Second {
			t.Fatalf("waited %v for a Retry-After of 3s", got)
		}
	}
	if got[0] <= 2*time.Second || got[0] > 3*time.Second {
		t.Fatalf("first wait = %v, want what is left of the 3s the registry named", got[0])
	}
}

// A registry that ignores Range makes the pull read the blob over one
// connection from then on.
func TestPullReadsWholeFromARegistryThatIgnoresRange(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, IgnoreRange: true}, 16, 6*miB)
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	requests := f.reg.Requests()
	if last := requests[len(requests)-1]; last.Range != "" {
		t.Fatalf("the last request still asked for %q: %+v", last.Range, requests)
	}
}

// A connection that dies mid-body is resumed at the byte it stopped at, a
// stalled one is cut, and neither leaves the pull short of a token.
func TestPullResumesBrokenAndStalledConnectionsWithoutLosingTokens(t *testing.T) {
	quickRecovery(t)
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 8 * miB}, 8, 8*miB)
	f.reg.SetFault(func(r fakeregistry.Request) *fakeregistry.Fault {
		switch r.N {
		case 2, 5:
			return &fakeregistry.Fault{AbortAfter: 200 << 10}
		case 3:
			return &fakeregistry.Fault{StallAfter: 100 << 10} // silent mid-body: the idle watch cuts it
		case 4:
			return &fakeregistry.Fault{Stall: true} // silent before the headers: the header timeout does
		}
		return nil
	})
	if _, err := f.pull(t, f.downloader(t, nil)); err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, r := range f.reg.Requests() {
		if r.N > 8 && strings.HasPrefix(r.Range, "bytes=") {
			resumed++
		}
	}
	if resumed == 0 {
		t.Fatalf("no request resumed after the broken ones: %+v", f.reg.Requests())
	}
}

// One connection that crawls far below its siblings is replaced even though it
// is above the absolute floor, and the replacement resumes where it stopped.
func TestPullRecyclesAConnectionFarBehindItsSiblings(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, PerConnRate: 4 * miB}, 8, 24*miB)
	var crawled atomic.Int64
	f.reg.SetFault(func(r fakeregistry.Request) *fakeregistry.Fault {
		if r.N == 2 && crawled.Add(1) == 1 {
			return &fakeregistry.Fault{Rate: 150 << 10} // 1/27 of its siblings, 150x the floor
		}
		return nil
	})
	started := time.Now()
	d := f.downloader(t, func(cfg *connbudget.SessionConfig) { cfg.SlowWindow = 150 * time.Millisecond })
	if _, err := f.pull(t, d); err != nil {
		t.Fatal(err)
	}
	if f.logged("reason=far below its siblings") == 0 {
		t.Fatalf("the crawling connection was not recognised by comparison: %v", f.logs)
	}
	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("took %s: the crawler was waited out", took)
	}
}

// When the whole link is slow every connection is slow together: nothing is
// far below its siblings, and nothing is recycled.
func TestPullKeepsEveryConnectionWhenTheWholeLinkIsSlow(t *testing.T) {
	f := newRegFixture(t, fakeregistry.Options{Redirect: true, AggregateRate: 1 * miB}, 4, 3*miB)
	d := f.downloader(t, func(cfg *connbudget.SessionConfig) {
		cfg.Fixed, cfg.Cap = 4, 4
		cfg.SlowWindow = 150 * time.Millisecond
	})
	if _, err := f.pull(t, d); err != nil {
		t.Fatal(err)
	}
	if n := f.logged("connection recycled"); n != 0 {
		t.Fatalf("%d connections recycled on a link that was merely slow: %v", n, f.logs)
	}
	for _, r := range f.reg.Requests() {
		if r.N > 4 && r.Range != "" {
			// 3 spans of 1 MiB each: any request beyond them is a resume.
			t.Fatalf("a connection was replaced: %+v", f.reg.Requests())
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                                0,
		"7":                               7 * time.Second,
		" 120 ":                           120 * time.Second,
		"-5":                              0,
		"soon":                            0,
		"Tue, 27 Oct 2026 12:00:30 GMT":   30 * time.Second,
		"Tue, 27 Oct 2026 11:00:00 GMT":   0, // already past
		"Tue, 27 Oct 2026 12:02:00 +0000": 0, // not an HTTP date
		"Tuesday, 27-Oct-26 12:01:00 GMT": 60 * time.Second,
		"Tue Oct 27 12:00:10 2026":        10 * time.Second,
	} {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMaxConnectionsFromEnv(t *testing.T) {
	for env, want := range map[string]int{"": DefaultMaxConnections, "0": DefaultMaxConnections, "junk": DefaultMaxConnections, "-3": DefaultMaxConnections, "1": 1, " 40 ": 40} {
		t.Setenv(MaxConnectionsEnv, env)
		if got := MaxConnectionsFromEnv(); got != want {
			t.Errorf("%s=%q: ceiling %d, want %d", MaxConnectionsEnv, env, got, want)
		}
	}
	t.Setenv(MaxConnectionsEnv, "5")
	if got := NewEngine(t.TempDir(), nil).Budget.Ceiling(); got != 5 {
		t.Fatalf("NewEngine ceiling = %d, want the configured 5", got)
	}
}

func TestPartsGrowWithTheBlobButStayBounded(t *testing.T) {
	old := rangeSpanTarget
	rangeSpanTarget = 64 << 20
	defer func() { rangeSpanTarget = old }()
	d := &Downloader{RangeParts: 4}
	for size, want := range map[int64]int{256 << 20: 4, 1 << 30: 16, 3 << 30: 48, 100 << 30: maxRangeParts} {
		if got := d.partsFor(size); got != want {
			t.Errorf("partsFor(%d MiB) = %d, want %d", size>>20, got, want)
		}
	}
}

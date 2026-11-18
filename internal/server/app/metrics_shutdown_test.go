package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunFlushesFinalProxyAccountingBeforeReturning is app.Run's shutdown
// sequencing test for the RunMetrics-vs-accountant race described on
// Run's metricsCtx comment: proxy.Accountant.Run's ctx.Done handler only
// enqueues its final flush into the metrics ingest queue partway through its
// own shutdown sequence (flush, close(stop), wait for its writer to drain
// and call the sink). If RunMetrics reacted to the same shutdown
// context, it could already have drained-and-exited before that enqueue,
// silently dropping it — Run must not return until this final flush has
// actually reached tsdb.
func TestRunFlushesFinalProxyAccountingBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath:    filepath.Join(dir, "gpuworks.db"),
		AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Wait for the listener so Run's background goroutines (the accountant
	// and the metrics ingest loop among them) are definitely started.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Mark a deployment dirty well within the accountant's own 10s
	// auto-flush interval, so the only thing that can flush it during this
	// test is the shutdown path being exercised.
	const deploymentID = uint(4242)
	a.proxyAcct.RecordImmediate(deploymentID, 200, false)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}

	series, err := a.obs.TSDB().QueryRange("gpuworks_proxy_requests_total",
		map[string]string{"deployment": strconv.FormatUint(uint64(deploymentID), 10)},
		0, time.Now().UnixMilli()+1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) == 0 {
		t.Fatal("the accountant's final flush (enqueued during shutdown) did not reach tsdb before Run returned")
	}
}

func TestRunFlushesRequestsFinishedAfterHTTPGraceExpires(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	const deploymentID = uint(4243)
	entered := make(chan struct{})
	// A real request finishes accounting only when the shutdown grace expires
	// and its socket closes. Accounting must still be running at that point.
	a.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		a.proxyAcct.RecordImmediate(deploymentID, 200, false)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.Addr() == "127.0.0.1:0" {
		t.Fatal("Run did not bind")
	}
	clientDone := make(chan struct{})
	go func() {
		response, _ := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + a.Addr() + "/")
		if response != nil {
			response.Body.Close()
		}
		close(clientDone)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Run did not finish")
	}
	<-clientDone
	series, err := a.obs.TSDB().QueryRange("gpuworks_proxy_requests_total",
		map[string]string{"deployment": "4243"}, 0, time.Now().UnixMilli()+1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Points) == 0 || series[0].Points[len(series[0].Points)-1].Value != 1 {
		t.Fatalf("request finishing during shutdown was not flushed: %+v", series)
	}
}

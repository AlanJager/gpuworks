package proxy

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Accountant.Run/writer-lifecycle tests. Classification and counter/flush
// unit tests live in accounting_test.go; HTTP-driven tests in
// accounting_e2e_test.go.

// TestNewAccountantUsesTheProductionQueueDepth exercises the exported
// constructor (app.go's only entry point into this package's accounting),
// as distinct from the unexported newAccountant used elsewhere in this file
// to pick a small queue depth for deterministic backpressure tests.
func TestNewAccountantUsesTheProductionQueueDepth(t *testing.T) {
	var mu sync.Mutex
	var got []tsdb.Sample
	a := NewAccountant(func(b []tsdb.Sample) {
		mu.Lock()
		got = append(got, b...)
		mu.Unlock()
	}, nil)
	if cap(a.queue) != sinkQueueDepth {
		t.Fatalf("queue capacity = %d, want %d", cap(a.queue), sinkQueueDepth)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	a.Dispatch(4)
	a.Finish(4, 200, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("expected NewAccountant's sink to receive the shutdown flush")
	}
}

func TestAccountantRunFlushesOnShutdown(t *testing.T) {
	var mu sync.Mutex
	var batches [][]tsdb.Sample
	sink := func(b []tsdb.Sample) {
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
	}
	a := newAccountant(sink, 4, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	a.Dispatch(9)
	a.Finish(9, 200, time.Millisecond)
	// Cancel well before the 10s periodic tick would fire, so any sample that
	// reaches the sink can only be the shutdown final flush.
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(batches) == 0 {
		t.Fatal("expected the shutdown final flush to reach the sink")
	}
	assertSample(t, batches[len(batches)-1], metricRequestsTotal,
		map[string]string{"deployment": "9", "class": "2xx", "node": "server"}, 1)
}

// TestAccountantRunSequentialInvocationsDoNotPanic is the N1/item-1
// regression test: Run used to close the shared sink queue on return, so a
// second Run call (testenv's RestartServer runs App.Run more than once on
// the same App, hence the same Accountant) would send on — and the third
// would close — an already-closed channel and crash the process. Run must be
// safely callable any number of times in sequence, each with its own final
// flush.
func TestAccountantRunSequentialInvocationsDoNotPanic(t *testing.T) {
	var mu sync.Mutex
	var batches [][]tsdb.Sample
	sink := func(b []tsdb.Sample) {
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
	}
	a := newAccountant(sink, 4, nil)

	runOnce := func(dep uint) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			a.Run(ctx) // must not panic even on the 2nd/3rd call
			close(done)
		}()
		a.Dispatch(dep)
		a.Finish(dep, 200, time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}

	runOnce(1)
	runOnce(2)
	runOnce(3) // a pre-fix Accountant would have panicked by now

	mu.Lock()
	defer mu.Unlock()
	for _, dep := range []string{"1", "2", "3"} {
		found := false
		for _, b := range batches {
			for _, s := range b {
				if s.Metric == metricRequestsTotal && s.Labels["deployment"] == dep {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("deployment %s's final flush never reached the sink across Run calls", dep)
		}
	}
}

// TestAccountantRunReturnsPromptlyDespiteStuckWriterAndGuardsRestart is the
// N3 regression test: a sink stuck forever used to keep Run blocked waiting
// for its writer goroutine, so App.Run's bounded shutdown wait would time
// out while Run itself never returned — and a subsequent RestartServer would
// then start a second, concurrent Run (and a second, concurrent writer) on
// the same Accountant. Run must now return as soon as its own stop signal is
// closed, and a following Run must wait (bounded) for the stuck writer
// before starting its own, logging if it gives up rather than hanging too.
func TestAccountantRunReturnsPromptlyDespiteStuckWriterAndGuardsRestart(t *testing.T) {
	block := make(chan struct{})
	sink := func([]tsdb.Sample) {
		<-block // simulate a sink stuck writing forever (e.g. a wedged tsdb)
	}
	buf := &syncBuffer{}
	a := newAccountant(sink, 4, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() {
		a.Run(ctx1)
		close(done1)
	}()
	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	cancel1() // final flush is enqueued; the writer picks it up and blocks in sink

	select {
	case <-done1:
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return promptly while its own writer was stuck in a slow sink")
	}

	// A second Run (simulating RestartServer) must not itself hang: it waits
	// briefly for the still-stuck first writer, gives up, logs, and proceeds
	// with its own writer rather than blocking a restart forever.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		a.Run(ctx2)
		close(done2)
	}()
	a.Dispatch(2)
	a.Finish(2, 200, time.Millisecond)
	cancel2()

	select {
	case <-done2:
	case <-time.After(previousWriterStopTimeout + 2*time.Second):
		t.Fatal("second Run did not return; it should give up waiting on the stuck previous writer and proceed")
	}
	if !hasLogMsg(buf, "proxy accounting: previous flush writer did not stop before this Run started; a stuck sink may still be running") {
		t.Fatalf("expected a warning that the previous writer did not stop in time; log:\n%s", buf.String())
	}

	close(block) // release both stuck sink calls so neither goroutine leaks
}

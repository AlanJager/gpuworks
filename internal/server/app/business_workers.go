package app

import (
	"context"
	"sync"
)

// businessWorkers are the loops that run beside the business handlers of one
// Run, or of one business generation of a cluster member: started together
// once the handlers can be reached, and stopped together, in the one order
// that lets each final write land before its store stops taking writes.
type businessWorkers struct {
	app *App
	// bg tracks every loop that runs on the shared ctx; see backgroundGroup
	// in shutdown.go (proxyAcct/RunMetrics track their own).
	bg            backgroundGroup
	acctCancel    context.CancelFunc
	acctDone      <-chan struct{}
	metricsCancel context.CancelFunc
	metricsDone   <-chan struct{}
	logsCancel    context.CancelFunc
	logsDone      <-chan struct{}
	stopped       sync.Once
}

// startBusinessWorkers starts the loops on ctx. They stop when ctx ends;
// stop waits for them.
func (a *App) startBusinessWorkers(ctx context.Context) *businessWorkers {
	// bg tracks every loop started below that runs on the shared ctx; see
	// backgroundGroup in shutdown.go (proxyAcct/RunMetrics track their own).
	w := &businessWorkers{app: a}
	goBG := w.bg.start

	// Background: flip stale-online nodes to offline as a safety net.
	goBG(func() { a.staleLoop(ctx) })
	goBG(func() { a.eventRetentionLoop(ctx) }) // Background: bound the deployment event timeline.
	goBG(func() { a.clusterNodeLoop(ctx) })    // Background: remove Kubernetes and Slurm nodes that left their cluster.
	// Background: reconnect nodes that listen for reverse server_dial sessions.
	goBG(func() { a.hub.RunServerDialReconnect(ctx, a.cfg.ServerDialReconnectInterval) })
	goBG(func() { a.hub.RunNodeQueues(ctx) }) // persist reports queued off the read loops
	// Background: the independent SSH path after ordinary reconnects exhaust.
	goBG(func() { a.recovery.Run(ctx) })
	// Background: reconcile deployments → instances.
	goBG(func() { a.runController(ctx) })
	goBG(func() { a.agentUpgrades.Run(ctx) })
	// Background: verify each PD prefill instance's KV handoff once.
	goBG(func() { runPDVerifier(ctx, a.store, a.proxy, a.obs.TSDB()) })
	// Background: schedules, waiting wakes, residency.
	waits, orders := a.ctrl.PlacementWaits(), a.ctrl.Orders()
	goBG(func() {
		runAutomation(ctx, a.store, a.proxy, a.proxyAcct, a.obs.hostMemory(), a.scaleReplicas, waits, orders, a.log)
	}) // Background: schedules, waiting wakes, residency.
	for _, run := range a.services() {
		goBG(func() { run(ctx) }) // Background: health checks, card records, usage report, notices, extension services.
	}
	// Background: the batches in progress, resumed from their checkpoints.
	if err := a.batches.Start(ctx); err != nil {
		a.log.Error("batch executor not started", "err", err)
	} else {
		goBG(func() { <-ctx.Done(); a.batches.Wait() })
	}
	// Background: prune metrics/logs beyond the retention window
	// (--metrics-retention, default 72h).
	//
	// RunMetrics deliberately runs on its OWN context (metricsCtx),
	// never the shared shutdown ctx: the proxy accountant's final flush (see
	// acctDone below) is only enqueued into the metrics ingest queue partway
	// through the accountant's OWN ctx.Done handling, which takes up to
	// proxy.finalFlushWaitTimeout. If RunMetrics reacted to the same
	// ctx, it would very likely have already drained whatever was queued at
	// that instant and returned - with nothing left running to ever pick up
	// the accountant's later enqueue, silently dropping it. Keeping it on a
	// separate context lets Run stop it explicitly, after stopProxyAccounting
	// confirms the accountant (and therefore its final enqueue) is done - see
	// stopMetricsIngest.
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	logsCtx, logsCancel := context.WithCancel(context.Background())
	metricsDone := make(chan struct{})
	logsDone := make(chan struct{})
	if a.obs != nil {
		goBG(func() { a.ops.run(ctx) })
		goBG(func() { a.ops.backfill(ctx) })
		goBG(func() { a.ops.replayScheduling(ctx) })
		go func() {
			defer close(logsDone)
			a.obs.RunLogs(logsCtx)
		}()
		go func() {
			defer close(metricsDone)
			a.obs.RunMetrics(metricsCtx)
		}()
		// Background: the server's own error-level slog records become
		// searchable server-source logs instead of living only in the
		// process's rotated file.
		goBG(func() { a.obs.RunSelfLogs(ctx, a.selfLog) })
	} else {
		close(metricsDone)
		close(logsDone)
	}
	// Accounting stops after requests have actually returned, including ones
	// whose HTTP connection was force-closed. Its final enqueue then precedes
	// the metrics loop's final drain (stopRun).
	acctCtx, acctCancel := context.WithCancel(context.Background())
	acctDone := make(chan struct{})
	go func() {
		defer close(acctDone)
		a.proxyAcct.Run(acctCtx)
	}()
	w.acctCancel, w.acctDone = acctCancel, acctDone
	w.metricsCancel, w.metricsDone = metricsCancel, metricsDone
	w.logsCancel, w.logsDone = logsCancel, logsDone
	return w
}

// stop waits for the loops after their ctx has ended and no handler can
// produce more work, then stops accounting, metrics and log ingest in that
// order. The App's stores stay open; Close releases them.
func (w *businessWorkers) stop() {
	w.stopped.Do(func() {
		a := w.app
		w.bg.wait() // Batch/automation producers may still complete gateway requests.
		w.acctCancel()
		a.stopProxyAccounting(w.acctDone)
		a.stopMetricsIngest(w.metricsCancel, w.metricsDone)
		w.logsCancel()
		<-w.logsDone
		a.flushRequestSinks()
	})
}

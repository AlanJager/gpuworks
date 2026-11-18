package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// stopRun is the one stop/drain order for cancellation and listener failure.
// App resources stay open after it; Close owns their final release.
func (a *App) stopRun(lns []*trackingListener, waitRequests func(), acctCancel context.CancelFunc, acctDone <-chan struct{}, metricsCancel context.CancelFunc, metricsDone <-chan struct{}, logsCancel context.CancelFunc, logsDone <-chan struct{}, bg *backgroundGroup) {
	a.proxy.StopHolding()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.http.Shutdown(shutCtx); err != nil {
		_ = a.http.Close()
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			a.log.Warn("forced HTTP shutdown", "err", err)
		}
	}
	closeConns(lns) // Shutdown leaves hijacked websocket connections open.
	a.hub.StopSessions()
	waitRequests()
	bg.wait() // Batch/automation producers may still complete gateway requests.
	acctCancel()
	a.stopProxyAccounting(acctDone)
	a.stopMetricsIngest(metricsCancel, metricsDone)
	logsCancel()
	<-logsDone
	a.flushRequestSinks()
}

// backgroundGroup tracks the loops Run starts on its shared ctx (other than
// accounting and ingest, which have their own established stop
// ordering - see stopProxyAccounting/stopMetricsIngest), so Run can wait for
// every one of them to actually return, on every exit path, before its
// deferred a.proxy.Close() runs (runPDVerifier uses a.proxy).
type backgroundGroup struct {
	wg sync.WaitGroup
}

// start runs fn in a goroutine tracked by the group.
func (g *backgroundGroup) start(fn func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn()
	}()
}

// wait blocks until every goroutine started by start has returned.
func (g *backgroundGroup) wait() {
	g.wg.Wait()
}

// stopProxyAccounting waits for the proxy accounting flush loop to
// return after ctx is done, so its final flush is fully enqueued into the
// metrics ingest queue before this App's caller is free to close the metrics
// store out from under it. It does NOT by itself guarantee that flush has
// reached tsdb: RunMetrics runs on its own context precisely so it
// can be stopped afterward (see stopMetricsIngest), once this enqueue is
// known to have already happened. The grace period is diagnostic; returning
// while a writer still uses an App-owned store would make final Close unsafe.
func (a *App) stopProxyAccounting(done <-chan struct{}) {
	if awaitWithTimeout(done, 2*time.Second) {
		a.log.Warn("proxy accounting did not stop within the shutdown grace period")
		<-done
	}
	a.proxyAcct.Wait() // Run's bounded grace can leave its final sink enqueue pending.
}

// stopMetricsIngest cancels RunMetrics's own context and waits
// for its final drain, so a proxy-accounting flush enqueued during
// stopProxyAccounting's wait is not silently dropped by an ingest loop that
// had already exited on the shared shutdown context. Must be called after
// stopProxyAccounting returns, never before or concurrently with it - see
// Run's metricsCtx comment for why the ordering itself is the fix. The wait
// is diagnostic: stores stay open until their ingest loop has returned.
func (a *App) stopMetricsIngest(cancel context.CancelFunc, done <-chan struct{}) {
	cancel()
	if awaitWithTimeout(done, 2*time.Second) {
		a.log.Warn("metrics ingest did not stop within the shutdown grace period")
		<-done
	}
}

// awaitWithTimeout waits for done to close, returning false, or gives up
// after timeout and returns true.
func awaitWithTimeout(done <-chan struct{}, timeout time.Duration) (timedOut bool) {
	select {
	case <-done:
		return false
	case <-time.After(timeout):
		return true
	}
}

// trackRunRequests retains ownership of requests after HTTP's graceful window
// expires: Server.Close closes sockets, but it does not join handlers. Once ctx
// is cancelled the admission lock prevents an Add racing a zero-count Wait.
func trackRunRequests(ctx context.Context, next http.Handler) (http.Handler, func()) {
	var admission sync.Mutex
	var work sync.WaitGroup
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admission.Lock()
		if ctx.Err() != nil {
			admission.Unlock()
			http.Error(w, "server is stopping", http.StatusServiceUnavailable)
			return
		}
		work.Add(1)
		admission.Unlock()
		defer work.Done()
		next.ServeHTTP(w, r)
	})
	return handler, func() {
		admission.Lock()
		defer admission.Unlock()
		work.Wait()
	}
}

// Package debugsrv is a signal-triggered, self-closing pprof endpoint.
//
// Production safety is the design constraint: the binary carries zero
// always-on debug surface. A SIGUSR1 opens a loopback-only listener serving
// the standard /debug/pprof handlers for a bounded window, then the whole
// listener is closed — new connections refused, live connections cut —
// without process restart. Another SIGUSR1 inside or after a window simply
// starts a fresh one.
package debugsrv

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// DefaultWindow bounds one opened session. Ten minutes covers several CPU
// profiles plus interactive poking with a small forgotten-window blast
// radius; operators can override at the CLI.
const DefaultWindow = 10 * time.Minute

// Start runs the signal loop until stop is closed. It never returns an
// error: a failed listen is logged and the next signal retries, because a
// diagnostics hook must not take the control plane down with it.
func Start(log *slog.Logger, window time.Duration, signals <-chan os.Signal, stop <-chan struct{}) {
	if window <= 0 {
		window = DefaultWindow
	}
	for {
		select {
		case <-stop:
			return
		case _, ok := <-signals:
			if !ok {
				return
			}
			// A repeat signal inside a window closes it and immediately
			// restarts a fresh one, so the loop keeps opening until a window
			// ends by timeout (or shutdown) rather than by signal.
			for openWindow(log, window, signals, stop) {
			}
		}
	}
}

// openWindow serves pprof on a fresh loopback listener until the window
// expires, stop closes, or another signal arrives. It reports whether the
// exit was signal-triggered (true = the caller should open a fresh window).
func openWindow(log *slog.Logger, window time.Duration, signals <-chan os.Signal, stop <-chan struct{}) bool {
	// Fixed port first so runbooks can rely on it; fall back to an
	// ephemeral one when 6060 is taken (another gpuworks on the host, or a
	// leftover window of a second process).
	ln, err := net.Listen("tcp", "127.0.0.1:6060")
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			log.Warn("pprof window: listen failed", "err", err)
			return false
		}
	}

	mux := http.NewServeMux()
	// The package's exported handler funcs keep the standard URLs without
	// touching http.DefaultServeMux — nothing in this process serves the
	// default mux, and that must stay true for this to be off by default.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()

	deadline := time.Now().Add(window)
	log.Info("pprof window open", "addr", "http://"+ln.Addr().String()+"/debug/pprof/", "closes_at", deadline.Format(time.RFC3339))

	timer := time.NewTimer(window)
	defer timer.Stop()
	signaled := false
	select {
	case <-timer.C:
	case <-stop:
	case <-signals:
		signaled = true
	}
	_ = srv.Close() // cuts live connections too, not just new ones
	<-done
	log.Info("pprof window closed", "addr", ln.Addr().String())
	return signaled
}

// Notify registers SIGUSR1 (and only it) so the rest of the process keeps
// owning SIGINT/SIGTERM semantics.
func Notify() chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	return ch
}

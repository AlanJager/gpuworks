//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

func (a *App) runClosedJoinedWithMaterialInterval(ctx context.Context, materialInterval time.Duration) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log := a.log
	if log == nil {
		log = slog.Default()
	}
	tlsConfig, err := a.cfg.listenerTLS()
	if err != nil {
		return err
	}
	lns, err := listenAll(a.cfg.bindings())
	if err != nil {
		return err
	}
	a.lnMu.Lock()
	if a.lns != nil {
		a.lnMu.Unlock()
		closeAll(lns)
		return errors.New("server app already running")
	}
	// The gate is the member's stable handler: business generations are
	// installed behind it and withdrawn from it while these sockets stay.
	server := &http.Server{Handler: a.http.Handler, ConnContext: connFunctions, TLSConfig: tlsConfig}
	a.http = server
	a.lns = lns
	a.lnMu.Unlock()
	defer func() {
		closeAll(lns)
		a.lnMu.Lock()
		a.lns = nil
		a.lnMu.Unlock()
	}()
	errc := make(chan error, len(lns))
	// Start the public listener before any quorum wait. Its stable gate keeps
	// business closed throughout classification and material construction.
	var serving backgroundGroup
	defer serving.wait()
	for _, ln := range lns {
		serving.start(func() {
			log.Info("server listening", "addr", ln.Addr().String(), "functions", ln.functions)
			var err error
			if tlsConfig != nil && publicTLSListener(ln) {
				err = server.ServeTLS(ln, "", "")
			} else {
				err = server.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		})
	}
	materialDone := make(chan error, 1)
	go func() { materialDone <- a.runJoinedMaterialLifecycle(ctx, materialInterval) }()
	materialReturned := false
	defer func() {
		cancel()
		// Close is only supported after Run: classification must finish before
		// Close reads the Store and original session ownership fields.
		if !materialReturned {
			resultErr = errors.Join(resultErr, <-materialDone)
		}
	}()
	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		closeConns(lns) // Shutdown leaves hijacked websocket connections open.
		return nil
	case err := <-errc:
		_ = server.Close()
		return err
	case err := <-materialDone:
		materialReturned = true
		_ = server.Close()
		return err
	}
}

func (a *App) runJoinedMaterialLifecycle(ctx context.Context, materialInterval time.Duration) (resultErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	initial := a.enrollment
	if initial != nil {
		if err := initial.Start(ctx); err != nil {
			if ctx.Err() != nil && err == ctx.Err() {
				return nil
			}
			return err
		}
		defer func() {
			shutdown, done := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
			defer done()
			if err := initial.Close(shutdown); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close initial joined enrollment: %w", err))
			}
		}()
	}
	if err := a.prepareJoinedMaterialRuntime(ctx); err != nil {
		if ctx.Err() != nil && err == ctx.Err() {
			return nil
		}
		return err
	}
	if prepared := a.enrollment; prepared != nil && prepared != initial {
		if err := prepared.Start(ctx); err != nil {
			if ctx.Err() != nil && err == ctx.Err() {
				return nil
			}
			return err
		}
		defer func() {
			shutdown, done := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
			defer done()
			if err := prepared.Close(shutdown); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close prepared joined enrollment: %w", err))
			}
		}()
	}
	if !a.legacyClosed {
		if err := a.startClosedMaterialEligibility(ctx); err != nil {
			return err
		}
		defer func() {
			shutdown, done := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
			defer done()
			if err := a.stopClosedMaterialEligibility(shutdown); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("stop joined material eligibility: %w", err))
			}
		}()
	}
	var control sync.WaitGroup
	if !a.legacyClosed && a.store.TransportAuthorizationEnabled() {
		control.Go(func() {
			runTransportAuthorization(ctx, a.closedTransportAuthorizationRefresh(a.store.RefreshTransportAuthorization), a.log, time.Second, 5*time.Second)
		})
	}
	if !a.legacyClosed {
		// Each complete material iteration belongs to enrollment's tracker.
		// Its bounded Close retains unresolved work for a later App.Close.
		go a.runMaterialReplication(ctx, materialInterval)
		control.Go(func() { a.runClosedBusiness(ctx) })
	}
	defer func() {
		cancel()
		control.Wait()
		shutdown, done := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
		defer done()
		if err := a.stopClosedBusiness(shutdown); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("stop joined business generation: %w", err))
		}
	}()
	if a.enrollment == nil {
		<-ctx.Done()
		return nil
	}
	wait := make(chan error, 1)
	go func() { wait <- a.enrollment.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-wait:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("joined material HTTPS server stopped unexpectedly")
		}
		return err
	}
}

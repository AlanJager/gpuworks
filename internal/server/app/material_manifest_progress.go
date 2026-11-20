//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"time"
)

const materialManifestProgressHeader = "X-Gpuworks-Material-Progress"

// Only authenticated manifest handlers call this after a completed stage. A
// stopped operation sends no keepalive; the peer's idle deadline stays active.
func writeMaterialManifestProgress(w http.ResponseWriter, r *http.Request, requests *materialRequestTracker) error {
	if r.Header.Get(materialManifestProgressHeader) != "1" {
		return nil
	}
	if err := r.Context().Err(); err != nil {
		return err
	}
	requests.mu.Lock()
	closed := requests.closed
	requests.mu.Unlock()
	if closed {
		return context.Canceled
	}
	// The enrollment listener also has an absolute 15s write deadline.
	// Renew it only after completed work, with the same idle window; otherwise
	// later 102 writes would fail even while the peer is receiving progress.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(materialPeerIdleTimeout)); err != nil {
		return err
	}
	// The final confirmation runs after receipt headers have been staged.
	// Informational responses must never carry that uncommitted receipt.
	headers := w.Header()
	source := headers.Get("X-Gpuworks-Material-Source")
	member := headers.Get("X-Gpuworks-Material-Member")
	digest := headers.Get("X-Gpuworks-Material-Digest")
	clearMaterialManifestReceipt(w)
	// WriteHeader flushes 1xx directly. Flush would commit an implicit 200.
	w.WriteHeader(http.StatusProcessing)
	if source != "" {
		headers.Set("X-Gpuworks-Material-Source", source)
	}
	if member != "" {
		headers.Set("X-Gpuworks-Material-Member", member)
	}
	if digest != "" {
		headers.Set("X-Gpuworks-Material-Digest", digest)
	}
	return nil
}

// Completed-call sampling bounds informational headers for large manifests.
// There is no heartbeat: an unfinished unit cannot renew the idle deadline.
func materialManifestProgressReporter(w http.ResponseWriter, r *http.Request, requests *materialRequestTracker, interval time.Duration) func() error {
	var last time.Time
	return func() error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		requests.mu.Lock()
		closed := requests.closed
		requests.mu.Unlock()
		if closed {
			return context.Canceled
		}
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < interval {
			return nil
		}
		if err := writeMaterialManifestProgress(w, r, requests); err != nil {
			return err
		}
		last = now
		return r.Context().Err()
	}
}

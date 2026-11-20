//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// Actual pinned mTLS and the production receiver verify receipt hints. The
// callbacks stand for the sender's native membership/repair boundary only;
// native tests below independently verify real persisted grant sequences.
func TestMaterialReceiptHintsConsumeCompletePinnedBytes(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	var items []materialBatchItem
	for i := range 32 {
		items = append(items, f.put([]byte(fmt.Sprintf("receipt hint %d", i))))
	}
	var checked []string
	repaired := 0
	err := flushMaterialReceiptHints(t.Context(), f.client, f.server.URL, 7, items,
		func(item materialBatchItem) error { checked = append(checked, item.Digest); return nil },
		func(materialBatchItem) error { repaired++; return errors.New("unexpected repair") })
	if err != nil || repaired != 0 || len(checked) != len(items) {
		t.Fatalf("checks=%d repair=%d err=%v", len(checked), repaired, err)
	}
	if !slices.Equal(checked, func() []string {
		var got []string
		for _, item := range items {
			got = append(got, item.Digest)
		}
		return got
	}()) {
		t.Fatal("per-file checks changed order")
	}
	requests := f.takeRequests()
	if len(requests) != 1 || !slices.Equal(requests[0], items) {
		t.Fatalf("requests=%v", requests)
	}
}

func TestMaterialReceiptHintsFailureRechecksEntireGroup(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "partial", "badmeta", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			items := []materialBatchItem{f.put([]byte("first canonical hint")), f.put([]byte("second canonical hint"))}
			switch mode {
			case "missing":
				if err := os.Remove(filepath.Join(f.root, items[1].Digest[:2], items[1].Digest)); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(filepath.Join(f.root, items[1].Digest[:2], items[1].Digest), bytes.Repeat([]byte("x"), int(items[1].Bytes)), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
					if mode == "unavailable" {
						http.Error(w, "not serving", 503)
						return true
					}
					recorded := httptest.NewRecorder()
					f.handler.ServeHTTP(recorded, r)
					raw := bytes.Clone(recorded.Body.Bytes())
					if mode == "partial" {
						raw = raw[:len(raw)-1]
					}
					for key, values := range recorded.Header() {
						w.Header()[key] = values
					}
					if mode == "badmeta" {
						w.Header().Set("X-Gpuworks-Material-Member", "8")
					}
					w.WriteHeader(recorded.Code)
					_, _ = w.Write(raw)
					return true
				}
			}
			var checks, fallback []string
			err := flushMaterialReceiptHints(t.Context(), f.client, f.server.URL, 7, items,
				func(item materialBatchItem) error { checks = append(checks, item.Digest); return nil },
				func(item materialBatchItem) error { fallback = append(fallback, item.Digest); return nil })
			if err != nil || len(checks) != 0 || len(fallback) != 2 || fallback[0] != items[0].Digest || fallback[1] != items[1].Digest {
				t.Fatalf("checks=%v fallback=%v err=%v", checks, fallback, err)
			}
			if len(f.takeRequests()) != 1 {
				t.Fatal("unexpected second batch")
			}
		})
	}
}

func TestMaterialReceiptHintsCancellationNeverFallsBack(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			item := f.put([]byte("cancel during actual complete response"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool { cancel(); <-r.Context().Done(); return true }
			} else {
				// An independent transport deadline must not become a receipt repair hint.
				original := f.client.Transport.(*http.Transport)
				tr := original.Clone()
				tr.RegisterProtocol("https", materialReadbackRoundTrip(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }))
				f.client.Transport = tr
			}
			var callbacks atomic.Int64
			err := flushMaterialReceiptHints(ctx, f.client, f.server.URL, 7, []materialBatchItem{item},
				func(materialBatchItem) error { callbacks.Add(1); return nil }, func(materialBatchItem) error { callbacks.Add(1); return nil })
			cause := context.Canceled
			if mode == "deadline" {
				cause = context.DeadlineExceeded
			}
			if !errors.Is(err, cause) || callbacks.Load() != 0 {
				t.Fatalf("callbacks=%d err=%v", callbacks.Load(), err)
			}
		})
	}
}

func TestMaterialReceiptHintsMembershipChangeStopsLaterChecks(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := []materialBatchItem{f.put([]byte("first unchanged replica")), f.put([]byte("second unchanged replica"))}
	changed := errors.New("actual membership changed")
	checks := 0
	err := flushMaterialReceiptHints(t.Context(), f.client, f.server.URL, 7, items,
		func(materialBatchItem) error { checks++; return changed }, func(materialBatchItem) error { t.Fatal("verified bytes entered fallback"); return nil })
	if !errors.Is(err, changed) || checks != 1 {
		t.Fatalf("checks=%d err=%v", checks, err)
	}
}

func TestMaterialReceiptHintsIndependentBatchDeadlineNeverRepairs(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	item := f.put([]byte("independent eight second batch budget"))
	f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	callbacks := 0
	err := flushMaterialReceiptHints(t.Context(), f.client, f.server.URL, 7, []materialBatchItem{item},
		func(materialBatchItem) error { callbacks++; return nil },
		func(materialBatchItem) error { callbacks++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || t.Context().Err() != nil || callbacks != 0 {
		t.Fatalf("batch error=%v parent error=%v callbacks=%d", err, t.Context().Err(), callbacks)
	}
	if len(f.takeRequests()) != 1 {
		t.Fatal("batch timeout retried")
	}
}

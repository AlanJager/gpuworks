//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmHintBudgetAndFailureContracts(t *testing.T) {
	for _, mode := range []string{"shared-budget", "caller-cancel", "wrapped-deadline", "partial-bytes", "no-yield"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			st := f.store
			items := []materialBatchItem{f.put([]byte("hint-contract-a")), f.put([]byte("hint-contract-b"))}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			identity, err := st.ClusterIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var checks, repairs, yields atomic.Int32
			fresh := func() error {
				_, admissions, err := st.GrantClusterTransportAdmissions(ctx, identity, f.senderKey)
				if err != nil {
					return err
				}
				if len(admissions) != 2 {
					return errors.New("incomplete actual fresh transport grant")
				}
				return nil
			}
			check := func(materialBatchItem) error { checks.Add(1); return fresh() }
			repair := func(item materialBatchItem) error {
				repairs.Add(1)
				if err := verifyRemoteMaterialReplica(ctx, f.client, f.server.URL, item.Digest, item.Bytes, f.memberID); err != nil {
					return err
				}
				return fresh()
			}
			var before func(context.Context) error
			var active *materialEligibility
			if mode == "shared-budget" {
				var calls atomic.Int32
				entered, done := make(chan struct{}), make(chan struct{})
				e := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
					if calls.Add(1) == 1 {
						err := verifyRemoteMaterialBatch(ctx, f.client, f.server.URL, items, f.memberID)
						return err == nil, err
					}
					close(entered)
					<-ctx.Done()
					return false, ctx.Err()
				}, materialReproofInterval)
				if ready, err := e.Reprove(ctx); !ready || err != nil {
					t.Fatalf("initial real bytes %v %v", ready, err)
				}
				go func() { defer close(done); _, _ = e.Reprove(ctx) }()
				<-entered
				active = e
				app := &App{memberState: memberState{materialEligibility: e}}
				before = func(ctx context.Context) error { yields.Add(1); return app.yieldWarmMaterialBatch(ctx) }
				t.Cleanup(func() { cancel(); e.Stop(); <-done })
			} else if mode != "no-yield" {
				before = func(waitCtx context.Context) error {
					yields.Add(1)
					if mode == "caller-cancel" {
						cancel()
						return nil
					}
					if mode == "wrapped-deadline" {
						return errors.Join(context.DeadlineExceeded, errors.New("additional scheduling error"))
					}
					return nil
				}
			}
			if mode == "partial-bytes" {
				f.mu.Lock()
				f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPost {
						return false
					}
					rec := httptest.NewRecorder()
					f.handler.ServeHTTP(rec, r)
					if rec.Code != http.StatusOK {
						t.Errorf("canonical grant/body status%d", rec.Code)
					}
					for key, values := range rec.Header() {
						for _, v := range values {
							w.Header().Add(key, v)
						}
					}
					w.WriteHeader(rec.Code)
					body := rec.Body.Bytes()
					_, _ = w.Write(body[:len(body)-1])
					return true
				}
				f.mu.Unlock()
			}
			started := time.Now()
			err = flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items, check, repair, before)
			elapsed := time.Since(started)
			switch mode {
			case "caller-cancel", "wrapped-deadline":
				if err == nil || checks.Load() != 0 || repairs.Load() != 0 || yields.Load() != 1 || len(f.takeRequests()) != 0 {
					t.Fatalf("failed scheduling admitted HTTP/check: err%v checks%d repairs%d yields%d", err, checks.Load(), repairs.Load(), yields.Load())
				}
				row, e := st.ClusterAdmissionForVerifiedKey(t.Context(), f.clusterID, f.senderKey)
				if e != nil || row.MaterialGrantSequence != 0 {
					t.Fatalf("failed prehint scheduling committed grant%d err%v", row.MaterialGrantSequence, e)
				}
			case "shared-budget":
				active.mu.Lock()
				stillPending := active.proofCtx != nil && active.proofCtx.Err() == nil
				active.mu.Unlock()
				if !stillPending || len(active.proofSlot) != 0 {
					t.Fatal("scheduling budget canceled active proof or changed its slot")
				}
				if err != nil || checks.Load() != 2 || repairs.Load() != 0 || yields.Load() != 3 {
					t.Fatalf("budget result err%v checks%d repairs%d yields%d", err, checks.Load(), repairs.Load(), yields.Load())
				}
				if elapsed < materialReproofInterval-50*time.Millisecond || elapsed > materialReproofInterval+500*time.Millisecond {
					t.Fatalf("same group restarted2s wait: %v", elapsed)
				}
				row, e := st.ClusterAdmissionForVerifiedKey(ctx, f.clusterID, f.senderKey)
				if e != nil || row.MaterialGrantSequence != 4 {
					t.Fatalf("real seed/hint+2 perfile grants%d err%v", row.MaterialGrantSequence, e)
				}
				assertCandidateRemoteSetReads(t, f.takeRequests(), map[string]int{items[0].Digest: 2, items[1].Digest: 2})
			case "partial-bytes":
				if err != nil || checks.Load() != 0 || repairs.Load() != 2 || yields.Load() != 3 {
					t.Fatalf("partial accepted prefix/skip repair: err%v checks%d repairs%d yields%d", err, checks.Load(), repairs.Load(), yields.Load())
				}
				row, e := st.ClusterAdmissionForVerifiedKey(ctx, f.clusterID, f.senderKey)
				if e != nil || row.MaterialGrantSequence != 5 {
					t.Fatalf("canonical partial+GET/perfile grants%d err%v", row.MaterialGrantSequence, e)
				}
				assertCandidateRemoteSetReads(t, f.takeRequests(), map[string]int{items[0].Digest: 2, items[1].Digest: 2})
			case "no-yield":
				if err != nil || checks.Load() != 2 || repairs.Load() != 0 || yields.Load() != 0 {
					t.Fatalf("no-yield compatibility: %v checks%d repairs%d yields%d", err, checks.Load(), repairs.Load(), yields.Load())
				}
				row, e := st.ClusterAdmissionForVerifiedKey(ctx, f.clusterID, f.senderKey)
				if e != nil || row.MaterialGrantSequence != 3 {
					t.Fatalf("no-yield actual grants%d err%v", row.MaterialGrantSequence, e)
				}
			}
		})
	}
}

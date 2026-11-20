//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeCandidateProofFailureDiagnosticsPreserveReadiness(t *testing.T) {
	for _, name := range []string{"received_manifest", "received_manifest_after_quorum", "remote_quorum", "canceled_before_proof"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			f := newCandidateGrantFixture(t, ctx)
			ready, metrics, err := f.prove(ctx)
			if !ready || err != nil || metrics.FailureStage != "" || metrics.FailureClass != "" || f.requests.Load() == 0 {
				t.Fatalf("real native/TLS baseline failed: ready=%t metrics=%+v err=%v", ready, metrics, err)
			}
			beforeRequests := f.requests.Load()
			removeManifest := func(index int) {
				t.Helper()
				digest := f.pointers[0].Digest
				if err := os.Remove(filepath.Join(f.receivers[index].root, digest[:2], digest)); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "received_manifest":
				removeManifest(0)
			case "received_manifest_after_quorum":
				// Remove only the local canonical copy after its first verification,
				// while an actual production TLS batch handler serves a valid remote.
				// The second local verification must detect the change.
				var onceErr error
				removed := false
				f.mu.Lock()
				f.before = func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if !removed {
						digest := f.pointers[0].Digest
						onceErr = os.Remove(filepath.Join(f.receivers[0].root, digest[:2], digest))
						removed = true
					}
				}
				f.mu.Unlock()
				defer func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if !removed || onceErr != nil {
						t.Errorf("actual local mutation missing: removed=%t err=%v", removed, onceErr)
					}
				}()
			case "remote_quorum":
				// Local files/index remain valid. Both actual remote handlers lack a
				// canonical manifest, so no complete remote copy can authorize readiness.
				removeManifest(1)
				removeManifest(2)
			case "canceled_before_proof":
				cancel()
			}
			ready, metrics, err = f.prove(ctx)
			if ready {
				t.Fatal("failed proof became ready")
			}
			if name == "canceled_before_proof" {
				if !errors.Is(err, context.Canceled) || metrics.FailureStage != "" || metrics.FailureClass != "" {
					t.Fatalf("early cancellation changed: metrics=%+v err=%v", metrics, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readiness must retain false,nil for non-context failure: %v", err)
			}
			wantStage := name
			if name == "received_manifest_after_quorum" {
				wantStage = "received_manifest"
			}
			if metrics.FailureStage != wantStage || metrics.FailureClass != "other" {
				t.Fatalf("actual checker diagnostic=%+v want=%s/other", metrics, wantStage)
			}
			if name == "received_manifest" {
				if metrics.RemoteBatch.Calls != 0 || f.requests.Load() != beforeRequests {
					t.Fatal("local failure unexpectedly reached remote quorum")
				}
			} else if metrics.ConsumerReferences.Calls != 2 || metrics.InstalledPaths.Calls != 1 || metrics.RemoteBatch.Calls < 1 || f.requests.Load() <= beforeRequests {
				t.Fatalf("failure did not reach actual final quorum/second verification: %+v", metrics)
			}
			if name == "remote_quorum" && metrics.RemoteBatch.VerifiedBytes != 0 {
				t.Fatal("incomplete remote manifest counted as verified")
			}
			raw, err := json.Marshal(metrics)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{f.receivers[0].root, f.keys[0], f.pointers[0].Digest, "shared tiny master", "tiny agent override", "missing or differs"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("failure diagnostic exposed private data: %s", raw)
				}
			}
		})
	}
}

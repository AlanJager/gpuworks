//go:build linux && dqlite

package app

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestCandidateProofMetricsCountVerifiedLocalBytesAndFailedAttempts(t *testing.T) {
	body := []byte("candidate proof verifies these bytes")
	digest := materialTestDigest(body)
	root := t.TempDir()
	if err := os.WriteFile(root+"/replica", body, 0600); err != nil {
		t.Fatal(err)
	}
	recorder := &candidateProofRecorder{}
	ctx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
	if err := verifyReceivedMaterial(ctx, root+"/replica", digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceivedMaterial(ctx, root+"/replica", digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	installed, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	if err := verifyRootMaterial(ctx, installed, "replica", digest, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceivedMaterial(ctx, root+"/replica", materialTestDigest([]byte("wrong")), int64(len(body))); err == nil {
		t.Fatal("damaged digest was accepted")
	}
	if got := recorder.snapshot().LocalHash; got.Calls != 4 || got.VerifiedBytes != 3*int64(len(body)) || got.Duration <= 0 {
		t.Fatalf("local hash metrics=%+v", got)
	}
	if got := (CandidateProofMetrics{}).LocalHash; got.Calls != 0 {
		t.Fatalf("unmeasured proof has metrics: %+v", got)
	}
	start := candidateProofStart(t.Context())
	if !start.IsZero() {
		t.Fatal("ordinary proof unexpectedly enabled metrics")
	}
	candidateProofObserve(t.Context(), proofLocalHash, time.Now(), 1)
	if got := recorder.snapshot().LocalHash.VerifiedBytes; got != 3*int64(len(body)) {
		t.Fatalf("unmeasured operation changed metrics: %d", got)
	}
}

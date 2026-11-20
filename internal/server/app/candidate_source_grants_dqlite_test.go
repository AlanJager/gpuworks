//go:build linux && dqlite

package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNativeCandidateProofBracketsEachSourceWithFreshGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	before := make([]store.ClusterAdmission, 3)
	for i, key := range f.keys {
		if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", key).First(&before[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ready, metrics, err := f.prove(ctx)
	if err != nil || !ready {
		t.Fatalf("real candidate proof: ready=%t metrics=%+v err=%v", ready, metrics, err)
	}
	// The local copy and one fully verified remote form a majority. A backup
	// may start within the existing proof budget if the primary is slow.
	completeSetBytes := f.pointers[0].Bytes + f.pointers[1].Bytes + int64(len("shared tiny master")+len("tiny agent override"))
	if metrics.MembershipGrant.Calls != 2 || f.requests.Load() < 1 || f.requests.Load() > 2 || metrics.RemoteBatch.Calls < 1 || metrics.RemoteBatch.Calls > 2 || metrics.RemoteBatch.VerifiedBytes < completeSetBytes || metrics.InstalledPaths.Calls != 1 {
		t.Errorf("proof/grant HTTP cost changed: grant=%d batch=%d verified=%d, want 2/1..2/at least %d", metrics.MembershipGrant.Calls, f.requests.Load(), metrics.RemoteBatch.VerifiedBytes, completeSetBytes)
	}
	for i := 1; i < 3; i++ {
		var after store.ClusterAdmission
		if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", f.keys[i]).First(&after).Error; err != nil {
			t.Fatal(err)
		}
		if delta := after.MaterialGrantSequence - before[i].MaterialGrantSequence; delta != 2 {
			t.Errorf("source %d durable fresh grants=%d, want 2", i, delta)
		}
	}
}

// Leftover manifest records from an earlier epoch must not poison the grant
// key set: a record whose source key has no admission, or whose admission was
// revoked, would be rejected by the grant and block every proof round.
func TestNativeCandidateProofSkipsSourceKeysWithoutLiveAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	ghost := strings.Repeat("e", 64)
	revokedKey := strings.Repeat("d", 64)
	var admin store.User
	if err := f.stores[0].DB().WithContext(ctx).Where("username = ?", "grant-proof-admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	_, token, err := f.stores[0].IssueClusterInvitation(ctx, expected.ClusterID, admin.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.stores[0].ClaimClusterInvitation(ctx, expected.ClusterID, token, revokedKey, "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	revoked, err := f.stores[0].RevokeClusterAdmission(ctx, expected.ClusterID, revokedKey)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("fixture admission was not revoked: %+v %v", revoked, err)
	}
	for i, staleKey := range []string{ghost, revokedKey} {
		record := store.MaterialManifestRecord{SourceMemberID: int64(424242 + i), ClusterID: expected.ClusterID, SourceKey: staleKey, Digest: f.pointers[0].Digest, Bytes: f.pointers[0].Bytes, RecordedAt: time.Now().UTC()}
		if err := f.stores[0].DB().WithContext(ctx).Create(&record).Error; err != nil {
			t.Fatalf("stale record %d: %#v", len(staleKey), err)
		}
	}
	keys, err := materialProofGrantKeys(ctx, f.stores[0], expected, f.keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || !slices.Contains(keys, f.keys[0]) || slices.Contains(keys, ghost) || slices.Contains(keys, revokedKey) {
		t.Fatalf("grant key set poisoned by stale records: %v", keys)
	}
	ready, metrics, err := f.prove(ctx)
	if !ready || err != nil || metrics.MembershipGrant.Calls != 2 {
		t.Fatalf("proof with stale source records: ready=%t grants=%d err=%v", ready, metrics.MembershipGrant.Calls, err)
	}
	var row store.ClusterAdmission
	if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", revokedKey).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RevokedAt == nil || row.MaterialGrantSequence != revoked.MaterialGrantSequence {
		t.Fatalf("revoked key granted or restored: %+v", row)
	}
}

func TestNativeCandidateProofAcceptsHighBitSourceID(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	const sourceID = uint64(1)<<63 | 42
	f := newCandidateGrantFixtureWithSourceID(t, ctx, sourceID)
	if f.databases[1].ID != sourceID || f.pointers[0].SourceMemberID >= 0 {
		t.Fatal("fixture lost high-bit native source ID")
	}
	ready, metrics, err := f.prove(ctx)
	if !ready || err != nil || metrics.MembershipGrant.Calls != 2 {
		t.Fatalf("valid uint64 source rejected: ready=%t grants=%d err=%v", ready, metrics.MembershipGrant.Calls, err)
	}
}

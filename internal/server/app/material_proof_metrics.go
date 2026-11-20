//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// CandidateProofStage counts attempted operations. VerifiedBytes counts only
// bytes whose digest and length were accepted; failed reads still contribute
// to Calls and Duration. Durations are cumulative and may overlap Total if a
// measured operation calls another measured operation.
type CandidateProofStage struct {
	Calls         int64
	VerifiedBytes int64
	Duration      time.Duration
}

// CandidateProofMetrics contains aggregate measurements for one on-demand
// candidate proof. It deliberately contains no member IDs, paths or digests.
type CandidateProofMetrics struct {
	FailureStage       string
	FailureClass       string
	Total              CandidateProofStage
	Enrollment         CandidateProofStage
	MembershipGrant    CandidateProofStage
	PointerCheck       CandidateProofStage
	LocalHash          CandidateProofStage
	RemoteGet          CandidateProofStage
	RemoteBatch        CandidateProofStage
	ConsumerReferences CandidateProofStage
	InstalledPaths     CandidateProofStage
}

// checkCandidateMaterialConsumerReferences retains only a static diagnostic
// when the readiness caller deliberately hides the consumer's original error.
// The ordinary checker and its returned error remain the readiness authority.
func checkCandidateMaterialConsumerReferences(ctx context.Context, st *store.Store, receiver *materialReceiver, manifest materialManifest) error {
	err := checkMaterialConsumerReferences(ctx, st, receiver, manifest)
	recordCandidateProofFailure(ctx, "consumer_references", err)
	return err
}

// verifyCandidateReceivedMaterialManifestBytes retains a fixed diagnostic while
// preserving the receiver verifier's exact manifest and error.
func verifyCandidateReceivedMaterialManifestBytes(ctx context.Context, st *store.Store, receiver *materialReceiver, expected store.ClusterIdentity, pointer store.MaterialManifestRecord) (materialManifest, error) {
	manifest, err := verifyReceivedMaterialManifestBytes(ctx, st, receiver, expected, pointer)
	recordCandidateProofFailure(ctx, "received_manifest", err)
	return manifest, err
}

// recordCandidateProofFailure retains only the first failure's fixed stage and
// bounded error class. The caller remains responsible for the original error.
func recordCandidateProofFailure(ctx context.Context, stage string, err error) {
	if err == nil {
		return
	}
	recorder, ok := ctx.Value(candidateProofMetricsKey{}).(*candidateProofRecorder)
	if !ok || recorder == nil {
		return
	}
	class := closedMemberErrorClass(err)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.metrics.FailureStage == "" {
		recorder.metrics.FailureStage = stage
		recorder.metrics.FailureClass = class
	}
}

type candidateProofMetricsKey struct{}

type candidateProofRecorder struct {
	mu      sync.Mutex
	metrics CandidateProofMetrics
}

func (recorder *candidateProofRecorder) snapshot() CandidateProofMetrics {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.metrics
}

type candidateProofStage uint8

const (
	proofEnrollment candidateProofStage = iota
	proofMembershipGrant
	proofPointerCheck
	proofLocalHash
	proofRemoteGet
	proofRemoteBatch
	proofConsumerReferences
	proofInstalledPaths
)

func candidateProofStart(ctx context.Context) time.Time {
	if recorder, ok := ctx.Value(candidateProofMetricsKey{}).(*candidateProofRecorder); !ok || recorder == nil {
		return time.Time{}
	}
	return time.Now()
}

func candidateProofObserve(ctx context.Context, stage candidateProofStage, started time.Time, verifiedBytes int64) {
	if started.IsZero() {
		return
	}
	recorder, ok := ctx.Value(candidateProofMetricsKey{}).(*candidateProofRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	metrics := &recorder.metrics
	var target *CandidateProofStage
	switch stage {
	case proofEnrollment:
		target = &metrics.Enrollment
	case proofMembershipGrant:
		target = &metrics.MembershipGrant
	case proofPointerCheck:
		target = &metrics.PointerCheck
	case proofLocalHash:
		target = &metrics.LocalHash
	case proofRemoteGet:
		target = &metrics.RemoteGet
	case proofRemoteBatch:
		target = &metrics.RemoteBatch
	case proofConsumerReferences:
		target = &metrics.ConsumerReferences
	case proofInstalledPaths:
		target = &metrics.InstalledPaths
	default:
		return
	}
	target.Calls++
	target.VerifiedBytes += verifiedBytes
	target.Duration += time.Since(started)
}

func grantMaterialProofMembership(ctx context.Context, st *store.Store, expected store.ClusterIdentity, key string) (uint64, []store.ClusterTransportMember, error) {
	started := candidateProofStart(ctx)
	revision, members, err := st.GrantClusterMembership(ctx, expected, key)
	candidateProofObserve(ctx, proofMembershipGrant, started, 0)
	return revision, members, err
}

// grantMaterialProofMembershipKeys observes one multi-key grant as a single
// membership grant stage: the proof's bracket cost no longer scales with the
// number of published sources.
func grantMaterialProofMembershipKeys(ctx context.Context, st *store.Store, expected store.ClusterIdentity, keys []string) (uint64, []store.ClusterTransportMember, error) {
	started := candidateProofStart(ctx)
	revision, members, err := st.GrantClusterMembershipKeys(ctx, expected, keys)
	candidateProofObserve(ctx, proofMembershipGrant, started, 0)
	return revision, members, err
}

func verifyCandidateRemoteReplica(ctx context.Context, client *http.Client, endpoint, digest string, size int64, memberID uint64) error {
	started := candidateProofStart(ctx)
	err := verifyRemoteMaterialReplica(ctx, client, endpoint, digest, size, memberID)
	verified := int64(0)
	if err == nil {
		verified = size
	}
	candidateProofObserve(ctx, proofRemoteGet, started, verified)
	return err
}

func verifyCandidateRemoteBatch(ctx context.Context, client *http.Client, endpoint string, files []materialBatchItem, memberID uint64) error {
	started := candidateProofStart(ctx)
	err := verifyRemoteMaterialBatch(ctx, client, endpoint, files, memberID)
	verified := int64(0)
	if err == nil {
		for _, file := range files {
			verified += file.Bytes
		}
	}
	candidateProofObserve(ctx, proofRemoteBatch, started, verified)
	return err
}

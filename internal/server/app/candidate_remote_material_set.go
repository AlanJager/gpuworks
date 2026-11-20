//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Each invocation proves one peer's bytes for this proof only. All source
// pointers remain required; shared canonical bytes are read once per peer.
func verifyCandidateRemoteMaterialSets(ctx context.Context, client *http.Client, endpoint string, pointers map[uint64]store.MaterialManifestRecord, manifests map[uint64]materialManifest, memberID uint64) error {
	return verifyCandidateRemoteMaterialSetsWithProgress(ctx, client, endpoint, pointers, manifests, memberID, nil)
}

func verifyCandidateRemoteMaterialSetsWithProgress(ctx context.Context, client *http.Client, endpoint string, pointers map[uint64]store.MaterialManifestRecord, manifests map[uint64]materialManifest, memberID uint64, progress func()) error {
	if len(pointers) == 0 || len(pointers) != len(manifests) {
		return errors.New("remote material sources are incomplete")
	}
	items := make([]materialBatchItem, 0)
	seen := make(map[string]int64)
	add := func(digest string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validMaterialDigest(digest) || size <= 0 || size > maxMaterialReplicaBytes {
			return errors.New("remote material source has invalid digest or size")
		}
		if prior, exists := seen[digest]; exists {
			if prior != size {
				return errors.New("remote material sources disagree on digest size")
			}
			return nil
		}
		seen[digest] = size
		items = append(items, materialBatchItem{Digest: digest, Bytes: size})
		return nil
	}
	// Gather before sending so a later source cannot contradict bytes that
	// were already accepted from an earlier batch.
	for _, sourceID := range slices.Sorted(maps.Keys(pointers)) {
		manifest, exists := manifests[sourceID]
		if !exists {
			return errors.New("remote material source manifest is missing")
		}
		pointer := pointers[sourceID]
		if err := add(pointer.Digest, pointer.Bytes); err != nil {
			return err
		}
		for _, file := range manifest.Files {
			if err := add(file.SHA256, file.Bytes); err != nil {
				return err
			}
		}
	}
	return verifyCandidateRemoteMaterialItemsWithProgress(ctx, client, endpoint, items, memberID, progress)
}

func verifyCandidateRemoteMaterialItems(ctx context.Context, client *http.Client, endpoint string, items []materialBatchItem, memberID uint64) error {
	return verifyCandidateRemoteMaterialItemsWithProgress(ctx, client, endpoint, items, memberID, nil)
}

func verifyCandidateRemoteMaterialItemsWithProgress(ctx context.Context, client *http.Client, endpoint string, items []materialBatchItem, memberID uint64, progress func()) error {
	batch := make([]materialBatchItem, 0, maxMaterialBatchCount)
	batchLimit := materialBatchLegacyCount
	batchBytes := int64(0)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		started := candidateProofStart(ctx)
		limit, err := verifyRemoteMaterialBatchLimit(ctx, client, endpoint, batch, memberID)
		verified := int64(0)
		if err == nil {
			verified = batchBytes
		}
		candidateProofObserve(ctx, proofRemoteBatch, started, verified)
		if err != nil {
			return err
		}
		if progress != nil {
			progress()
		}
		batchLimit = limit
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.Bytes > maxMaterialBatchBytes {
			if err := flush(); err != nil {
				return err
			}
			if err := verifyCandidateRemoteReplica(ctx, client, endpoint, item.Digest, item.Bytes, memberID); err != nil {
				return err
			}
			if progress != nil {
				progress()
			}
			continue
		}
		if len(batch) == batchLimit || batchBytes+item.Bytes > maxMaterialBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, item)
		batchBytes += item.Bytes
	}
	return flush()
}

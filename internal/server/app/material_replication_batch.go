//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Uploads retain their individual durable acknowledgements. Readback batches
// prove all bytes before any new receipt from that batch is recorded; each
// receipt still follows its own fresh check of the exact admitted peer.
func replicateMaterialInventoryToPeer(ctx context.Context, st *store.Store, client *http.Client, member store.ClusterTransportMember, expected store.ClusterIdentity, localKey string, files []MaterialFile, local map[string]string, onTransfer func()) (bool, error) {
	return replicateMaterialInventoryToPeerWithYield(ctx, st, client, member, expected, localKey, files, local, onTransfer, nil)
}

func replicateMaterialInventoryToPeerWithYield(ctx context.Context, st *store.Store, client *http.Client, member store.ClusterTransportMember, expected store.ClusterIdentity, localKey string, files []MaterialFile, local map[string]string, onTransfer func(), beforeWarmBatch func(context.Context) error) (bool, error) {
	transferredAny := false
	// Upload readback retains the legacy size without capability negotiation.
	pending := make([]materialBatchItem, 0, materialBatchLegacyCount)
	pendingBytes := int64(0)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		var err error
		// A single new or repaired file keeps the original readback path. Large
		// files cannot enter the bounded batch protocol.
		if len(pending) == 1 {
			file := pending[0]
			err = verifyRemoteMaterialReplica(ctx, client, member.EnrollmentOrigin, file.Digest, file.Bytes, member.ID)
		} else {
			err = verifyRemoteMaterialBatch(ctx, client, member.EnrollmentOrigin, pending, member.ID)
		}
		if err != nil {
			return fmt.Errorf("read back material batch from member %d: %w", member.ID, err)
		}
		receipts := make([]store.MaterialRemoteReceipt, 0, len(pending))
		for _, file := range pending {
			if err := requireCurrentMaterialPeer(ctx, st, member, expected, localKey); err != nil {
				return err
			}
			receipts = append(receipts, store.MaterialRemoteReceipt{Digest: file.Digest, MemberID: int64(member.ID), Bytes: file.Bytes})
		}
		if err := st.RecordMaterialRemoteReceipts(ctx, receipts); err != nil {
			return err
		}
		pending = pending[:0]
		pendingBytes = 0
		return nil
	}
	upload := func(file materialBatchItem) error {
		if len(pending) == materialBatchLegacyCount || pendingBytes+file.Bytes > maxMaterialBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		ack, err := sendMaterialReplica(ctx, client, member.EnrollmentOrigin, local[file.Digest], file.Digest, member.ID)
		if err != nil {
			return fmt.Errorf("replicate material to member %d: %w", member.ID, err)
		}
		if onTransfer != nil {
			onTransfer()
		}
		if ack.Bytes != file.Bytes {
			return errors.New("material receipt does not match inventory size")
		}
		transferredAny = true
		pending = append(pending, file)
		pendingBytes += file.Bytes
		if file.Bytes > maxMaterialBatchBytes {
			return flush()
		}
		return nil
	}
	// Replica rows only nominate bytes for verification. Their paths are remote
	// metadata and must never be opened by the sender.
	indexHints := make(map[string]bool)
	verifiedHints := make([]store.MaterialRemoteReceipt, 0, materialBatchLegacyCount)
	recordHints := func() error {
		if len(verifiedHints) == 0 {
			return nil
		}
		// A fallback group may have uploaded repairs alongside verified hints.
		// Finish their durable acknowledgements and readback before promoting
		// any new hint from that group to a receipt.
		if err := flush(); err != nil {
			return err
		}
		if err := st.RecordMaterialRemoteReceipts(ctx, verifiedHints); err != nil {
			return err
		}
		verifiedHints = verifiedHints[:0]
		return nil
	}
	checkPeer := func(file materialBatchItem) error {
		if err := requireCurrentMaterialPeer(ctx, st, member, expected, localKey); err != nil {
			return err
		}
		if indexHints[file.Digest] {
			verifiedHints = append(verifiedHints, store.MaterialRemoteReceipt{Digest: file.Digest, MemberID: int64(member.ID), Bytes: file.Bytes})
		}
		return nil
	}
	// Receipts and replica rows are only hints. A failed group returns to the
	// original per-file pinned HEAD and upload/readback path.
	recheck := func(file materialBatchItem) error {
		err := probeRemoteMaterialReplica(ctx, client, member.EnrollmentOrigin, file.Digest, file.Bytes, member.ID)
		if err == nil {
			return checkPeer(file)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if unavailableMaterialPeer(err) {
			// No verdict on the bytes: an upload here would count as a change.
			return fmt.Errorf("probe material on member %d: %w", member.ID, err)
		}
		return upload(file)
	}
	hints := make([]materialBatchItem, 0, materialBatchLegacyCount)
	hintBytes := int64(0)
	flushHints := func() error {
		if err := flushMaterialReceiptHintsWithYield(ctx, client, member.EnrollmentOrigin, member.ID, hints, checkPeer, recheck, beforeWarmBatch); err != nil {
			return err
		}
		// No new index-hint receipt escapes a partially checked group. Repair
		// uploads retain their separate acknowledgement/readback receipt path.
		if err := recordHints(); err != nil {
			return err
		}
		hints = hints[:0]
		hintBytes = 0
		return nil
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return transferredAny, err
		}
		if _, duplicate := seen[file.SHA256]; duplicate {
			continue
		}
		seen[file.SHA256] = struct{}{}
		source := local[file.SHA256]
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() {
			return transferredAny, fmt.Errorf("local %s material is unavailable: %v", file.Class, err)
		}
		hasReceipt, err := st.HasMaterialRemoteReceipt(ctx, file.SHA256, member.ID, info.Size())
		if err != nil {
			return transferredAny, err
		}
		hasHint := hasReceipt
		if !hasReceipt {
			count, err := st.CountMaterialReplicas(ctx, file.SHA256, int64(member.ID))
			if err != nil {
				return transferredAny, err
			}
			hasHint = count != 0
			indexHints[file.SHA256] = hasHint
		}
		item := materialBatchItem{Digest: file.SHA256, Bytes: info.Size()}
		if hasHint && item.Bytes > 0 && item.Bytes <= maxMaterialBatchBytes {
			if len(hints) == materialBatchLegacyCount || hintBytes+item.Bytes > maxMaterialBatchBytes {
				if err := flushHints(); err != nil {
					return transferredAny, err
				}
			}
			hints = append(hints, item)
			hintBytes += item.Bytes
			continue
		}
		if err := flushHints(); err != nil {
			return transferredAny, err
		}
		if hasHint {
			err = recheck(item)
		} else {
			err = upload(item)
		}
		if err != nil {
			return transferredAny, err
		}
		if err := recordHints(); err != nil {
			return transferredAny, err
		}
	}
	if err := flushHints(); err != nil {
		return transferredAny, err
	}
	return transferredAny, flush()
}

func requireCurrentMaterialPeer(ctx context.Context, st *store.Store, member store.ClusterTransportMember, expected store.ClusterIdentity, localKey string) error {
	_, current, err := st.GrantClusterMembership(ctx, expected, localKey)
	if err != nil {
		return err
	}
	for _, candidate := range current {
		if candidate.ID == member.ID && candidate.KeyFingerprint == member.KeyFingerprint && candidate.EnrollmentOrigin == member.EnrollmentOrigin {
			return nil
		}
	}
	return fmt.Errorf("material member %d changed during transfer", member.ID)
}

// Hint batches use the legacy bounds, independently of proof-side capability
// negotiation. Successful full-byte verification still requires every original
// per-file current-member check; a failed group provides no accepted prefix.
func flushMaterialReceiptHints(ctx context.Context, client *http.Client, origin string, memberID uint64, hints []materialBatchItem, checkPeer, recheck func(materialBatchItem) error) error {
	return flushMaterialReceiptHintsWithYield(ctx, client, origin, memberID, hints, checkPeer, recheck, nil)
}

func flushMaterialReceiptHintsWithYield(ctx context.Context, client *http.Client, origin string, memberID uint64, hints []materialBatchItem, checkPeer, recheck func(materialBatchItem) error, beforeWarmBatch func(context.Context) error) error {
	if len(hints) == 0 {
		return nil
	}
	remainingWait := materialReproofInterval
	yield := func() error {
		if beforeWarmBatch == nil {
			return nil
		}
		started := time.Now()
		waitCtx, cancel := context.WithTimeout(ctx, max(0, remainingWait))
		waitErr := beforeWarmBatch(waitCtx)
		childErr := waitCtx.Err()
		cancel()
		remainingWait -= time.Since(started)
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only this scheduling child's exact deadline ends the wait.
		// Wrapped or combined errors must still reject the next grant.
		if waitErr != nil && !(waitErr == context.DeadlineExceeded && childErr == context.DeadlineExceeded) {
			return waitErr
		}
		return nil
	}
	// The hint HTTP also obtains a fresh durable material grant. Yield before
	// it as well as before each file check, sharing the original group budget.
	if err := yield(); err != nil {
		return err
	}
	err := verifyRemoteMaterialBatch(ctx, client, origin, hints, memberID)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	check := checkPeer
	if err != nil {
		check = recheck
	}
	for _, file := range hints {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(); err != nil {
			return err
		}
		if err := check(file); err != nil {
			return err
		}
	}
	return nil
}

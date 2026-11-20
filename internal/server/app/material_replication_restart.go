//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// errMaterialSourceUnpublished is the restart check's answer for a source that
// has published no manifest yet, as a member that has just joined. There is
// nothing to compare, so the cycle replicates in full; it is not a finding
// about material and must not close a proof made since.
var errMaterialSourceUnpublished = errors.New("material source has published no manifest")

// checkMaterialReplicationRestart restores scheduling observations only. It
// never publishes material, installs consumers, or grants business readiness.
// Public authorization/phase revisions and the full pointer detect intervening
// public transitions. Like a normal replication cycle, this does not promise
// detection of native configuration ABA or restoration by arbitrary raw SQL.
func checkMaterialReplicationRestart(ctx context.Context, dataDir string, st *store.Store, bundle *enrollmentBundle, sourceID uint64, sourceKey string, expected store.ClusterIdentity, members []store.ClusterTransportMember) (store.MaterialManifestRecord, error) {
	return checkMaterialReplicationRestartWithGrant(ctx, dataDir, st, bundle, sourceID, sourceKey, expected, members, func(ctx context.Context) (uint64, []store.ClusterTransportMember, error) {
		return st.GrantClusterMembership(ctx, expected, sourceKey)
	})
}

func checkMaterialReplicationRestartWithGrant(ctx context.Context, dataDir string, st *store.Store, bundle *enrollmentBundle, sourceID uint64, sourceKey string, expected store.ClusterIdentity, members []store.ClusterTransportMember, grant func(context.Context) (uint64, []store.ClusterTransportMember, error)) (result store.MaterialManifestRecord, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if st == nil || sourceID == 0 || len(members) < 2 || grant == nil || !slices.ContainsFunc(members, func(m store.ClusterTransportMember) bool {
		return m.ID == sourceID && m.KeyFingerprint == sourceKey && bundle != nil && m.EnrollmentOrigin == bundle.info.PublicURL
	}) {
		return result, errors.New("material restart requires the current source and peers")
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return result, err
	}
	defer func() {
		rootErr, closeErr, canceled := checkMaterialDataRoot(dataDir, root), root.Close(), ctx.Err()
		if rootErr != nil || closeErr != nil || canceled != nil {
			err = errors.Join(err, rootErr, closeErr, canceled)
		}
		if err != nil {
			result = store.MaterialManifestRecord{}
		}
	}()
	if err := checkMaterialDataRoot(dataDir, root); err != nil {
		return result, err
	}
	revision, current, err := grant(ctx)
	if err != nil {
		return result, err
	}
	if revision == 0 || !slices.Equal(current, members) {
		return result, errors.New("material restart membership differs from observation")
	}
	phase, err := st.ReadMaintenancePhase(ctx)
	if err != nil {
		return result, err
	}
	if phase.Phase != store.MaintenanceServing {
		return result, store.ErrMaintenanceNotServing
	}
	pointer, err := st.MaterialManifestBySource(ctx, int64(sourceID))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, errMaterialSourceUnpublished
	}
	if err != nil {
		return result, err
	}
	if pointer.ClusterID != expected.ClusterID || pointer.SourceKey != sourceKey || !validMaterialDigest(pointer.Digest) || pointer.Bytes <= 0 || pointer.Bytes > maxMaterialManifestBytes || pointer.RecordedAt.IsZero() {
		return result, errors.New("material restart pointer differs from current source")
	}
	canonical, err := filepath.Abs(dataDir)
	if err != nil {
		return result, err
	}
	receiver, err := newMaterialReceiver(filepath.Join(canonical, "material-replicas"), sourceID)
	if err != nil {
		return result, err
	}
	if err := checkLocalRestartMaterial(ctx, dataDir, st, receiver, expected, pointer); err != nil {
		return result, err
	}
	// No SQL snapshot/transaction is kept open while a peer verifies its own
	// current bytes, index and installed consumer paths over pinned mutual TLS.
	for _, member := range members {
		if member.ID == sourceID {
			continue
		}
		client, err := memberMaterialClient(bundle, member.EnrollmentOrigin, member.KeyFingerprint)
		if err != nil {
			return result, err
		}
		err = checkRemoteInstalledMaterialManifest(ctx, client, member.EnrollmentOrigin, pointer.Digest, sourceID, member.ID)
		client.CloseIdleConnections()
		if err != nil {
			// A partitioned member's refusal is not evidence about these bytes;
			// the repair run that follows skips it like any unavailable peer.
			return result, explainMemberReachability(ctx, err, member.Address)
		}
	}
	// Repeat every local validation after HTTP; a successful earlier parse or
	// hash is not evidence that mutable source/consumer files stayed unchanged.
	if err := checkLocalRestartMaterial(ctx, dataDir, st, receiver, expected, pointer); err != nil {
		return result, err
	}
	finalRevision, finalMembers, err := grant(ctx)
	if err != nil {
		return result, err
	}
	finalPhase, err := st.ReadMaintenancePhase(ctx)
	if err != nil {
		return result, err
	}
	latest, err := st.MaterialManifestBySource(ctx, int64(sourceID))
	if err != nil {
		return result, err
	}
	if finalRevision != revision || !slices.Equal(finalMembers, members) || finalPhase != phase || !sameBootstrapMaterialPointers([]store.MaterialManifestRecord{pointer}, []store.MaterialManifestRecord{latest}) {
		return result, errors.New("material restart authorization, phase or pointer changed")
	}
	return pointer, nil
}

func checkLocalRestartMaterial(ctx context.Context, dataDir string, st *store.Store, receiver *materialReceiver, expected store.ClusterIdentity, pointer store.MaterialManifestRecord) error {
	manifestPath := filepath.Join(receiver.root, pointer.Digest[:2], pointer.Digest)
	if err := checkReceivedMaterialRow(ctx, st, receiver, pointer.Digest, manifestPath); err != nil {
		return err
	}
	raw, err := readReceivedMaterial(ctx, manifestPath, pointer.Digest, pointer.Bytes)
	if err != nil {
		return err
	}
	manifest, err := parseMaterialManifest(raw, expected, receiver.memberID, pointer.SourceKey)
	if err != nil {
		return err
	}
	inventory, err := InspectRequiredMaterials(ctx, dataDir, st)
	if err != nil {
		return err
	}
	paths := make(map[string]string, len(inventory.Files))
	for _, file := range inventory.Files {
		paths[file.SHA256] = filepath.Join(receiver.root, file.SHA256[:2], file.SHA256)
	}
	current, err := buildMaterialManifest(dataDir, inventory, paths, expected, receiver.memberID, pointer.SourceKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, raw) {
		return errors.New("material restart inventory differs from published bytes")
	}
	digests := []string{pointer.Digest}
	verified := map[string]bool{pointer.Digest: true}
	for _, file := range manifest.Files {
		if !verified[file.SHA256] {
			verified[file.SHA256] = true
			digests = append(digests, file.SHA256)
		}
	}
	if err := checkReceivedMaterialRows(ctx, st, receiver, digests); err != nil {
		return err
	}
	clear(verified)
	for _, file := range manifest.Files {
		if verified[file.SHA256] {
			continue
		}
		if err := verifyReceivedMaterial(ctx, filepath.Join(receiver.root, file.SHA256[:2], file.SHA256), file.SHA256, file.Bytes); err != nil {
			return err
		}
		verified[file.SHA256] = true
	}
	if err := checkMaterialConsumerReferences(ctx, st, receiver, manifest); err != nil {
		return err
	}
	if err := checkInstalledMaterialManifest(ctx, receiver, manifest); err != nil {
		return err
	}
	return checkReceivedMaterialRows(ctx, st, receiver, digests)
}

//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replicateRequiredMaterials copies the current local inventory to reachable
// admitted peers with registered HTTPS origins. A receipt is committed
// only after the receiver has returned its durable acknowledgement and a
// separate authenticated read has verified the complete remote bytes.
func replicateRequiredMaterials(ctx context.Context, dataDir string, st *store.Store, bundle *enrollmentBundle, installedDigest string, forceInstall bool, onChange func(), log *slog.Logger) error {
	return replicateRequiredMaterialsWithYield(ctx, dataDir, st, bundle, installedDigest, forceInstall, onChange, log, nil)
}

func replicateRequiredMaterialsWithYield(ctx context.Context, dataDir string, st *store.Store, bundle *enrollmentBundle, installedDigest string, forceInstall bool, onChange func(), log *slog.Logger, beforeWarmBatch func(context.Context) error) error {
	changed := false
	markChanged := func() {
		if !changed {
			changed = true
			onChange()
		}
	}
	routeID, localKey, expected, members, err := materialSenderMembers(ctx, st, bundle)
	if err != nil {
		return err
	}
	if len(members) < 2 {
		return nil
	}
	inventory, err := InspectRequiredMaterials(ctx, dataDir, st)
	if err != nil {
		return err
	}
	local, err := persistLocalMaterialInventory(ctx, dataDir, st, routeID, inventory)
	if err != nil {
		return err
	}
	manifestBytes, err := buildMaterialManifest(dataDir, inventory, local, expected, routeID, localKey)
	if err != nil {
		return err
	}
	manifestHash := sha256.Sum256(manifestBytes)
	manifestDigest := hex.EncodeToString(manifestHash[:])
	manifestPath, err := persistLocalMaterialBytes(ctx, dataDir, st, routeID, manifestDigest, manifestBytes)
	if err != nil {
		return err
	}
	// Keep manifest bytes canonical; only the temporary transfer order changes.
	files := make([]MaterialFile, 0, len(inventory.Files))
	for _, critical := range []bool{true, false} {
		for _, file := range inventory.Files {
			if (file.Class == MaterialMemberIdentity || file.Class == MaterialMasterSecret) == critical {
				files = append(files, file)
			}
		}
	}
	transferredAny := false
	// silent names each member skipped as unreachable and why, for the error
	// of a pass that then finds too few members verified.
	var silent []string
	verifiedPeers := make([]store.ClusterTransportMember, 0, len(members)-1)
	publishedMajority := false
	installedPeers := 0
	publishAndInstall := func() error {
		if !publishedMajority {
			// The planning cache can be empty after a restart or failed repair. Only
			// the current complete publication decides whether this is a new pointer.
			published, publicationErr := st.MaterialManifestBySource(ctx, int64(routeID))
			if publicationErr != nil && !errors.Is(publicationErr, gorm.ErrRecordNotFound) {
				if !materialCheckpointContention(publicationErr) {
					markChanged()
				}
				return publicationErr
			}
			if publicationErr != nil || published.SourceMemberID != int64(routeID) || published.ClusterID != expected.ClusterID || published.SourceKey != localKey || published.Digest != manifestDigest || published.Bytes != int64(len(manifestBytes)) {
				markChanged()
			}
			if err := st.PublishMaterialManifest(ctx, expected, routeID, localKey, manifestDigest, int64(len(manifestBytes)), verifiedPeers); err != nil {
				return err
			}
			publishedMajority = true
		}

		if !shouldMaterializeManifest(forceInstall, installedDigest, manifestDigest, transferredAny) {
			return nil
		}
		for _, member := range verifiedPeers[installedPeers:] {
			client, err := memberMaterialClient(bundle, member.EnrollmentOrigin, member.KeyFingerprint)
			if err != nil {
				return err
			}
			err = confirmRemoteMaterialManifest(ctx, client, member.EnrollmentOrigin, manifestDigest, routeID, member.ID)
			if err == nil {
				err = materializeRemoteMaterialManifest(ctx, client, member.EnrollmentOrigin, manifestDigest, routeID, member.ID)
			}
			client.CloseIdleConnections()
			if err != nil {
				return fmt.Errorf("member %d did not complete material handoff: %w", member.ID, err)
			}
		}
		installedPeers = len(verifiedPeers)
		return nil
	}
	for _, member := range members {
		if member.ID == routeID {
			continue
		}
		err := func() error {
			if member.EnrollmentOrigin == "" {
				return fmt.Errorf("live material member %d has no HTTPS origin", member.ID)
			}
			client, err := memberMaterialClient(bundle, member.EnrollmentOrigin, member.KeyFingerprint)
			if err != nil {
				return err
			}
			defer client.CloseIdleConnections()
			// Early durable bytes are not a receipt, publication, or handoff.
			manifestTransferred, err := ensureRemoteMaterialReplica(ctx, st, client, member.EnrollmentOrigin, manifestPath, manifestDigest, int64(len(manifestBytes)), member.ID, markChanged)
			if err != nil {
				return fmt.Errorf("replicate manifest to member %d: %w", member.ID, err)
			}
			transferred, err := replicateMaterialInventoryToPeerWithYield(ctx, st, client, member, expected, localKey, files, local, markChanged, beforeWarmBatch)
			if err != nil {
				return err
			}
			transferredAny = transferredAny || transferred
			_, current, err := st.GrantClusterMembership(ctx, expected, localKey)
			if err != nil {
				return err
			}
			stillCurrent := false
			for _, candidate := range current {
				if candidate.ID == member.ID && candidate.KeyFingerprint == member.KeyFingerprint && candidate.EnrollmentOrigin == member.EnrollmentOrigin {
					stillCurrent = true
					break
				}
			}
			if !stillCurrent {
				return fmt.Errorf("material member %d changed during manifest transfer", member.ID)
			}
			if manifestTransferred {
				transferredAny = true
				if err := st.RecordMaterialRemoteReceipt(ctx, manifestDigest, member.ID, int64(len(manifestBytes))); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			err = explainMemberReachability(ctx, err, member.Address)
		}
		if peerUnreachable(err) {
			if log != nil {
				log.Warn("cluster member is unreachable", "member", member.ID, "database", member.Address, "err", err)
			}
			silent = append(silent, silentMaterialMember(member, err))
			continue
		}
		if err != nil {
			return err
		}
		verifiedPeers = append(verifiedPeers, member)
		// This count only schedules the attempt. PublishMaterialManifest obtains
		// its own fresh membership and validates every receipt before committing.
		// Hand the verified majority to consumers before a remaining peer can
		// stall or refuse replication; its later failure is still returned.
		if 1+len(verifiedPeers) > len(members)/2 {
			if err := publishAndInstall(); err != nil {
				return err
			}
		}
	}
	if !publishedMajority {
		return namingSilentMaterialMembers(publishAndInstall(), silent)
	}
	return nil
}

func shouldMaterializeManifest(force bool, installedDigest, currentDigest string, transferred bool) bool {
	return force || installedDigest != currentDigest || transferred
}

func materialInstallationDue(now, lastInstall time.Time, interval time.Duration, sameMembers bool) bool {
	return !sameMembers || lastInstall.IsZero() || now.Before(lastInstall) || now.Sub(lastInstall) >= 15*interval
}

func materialSenderMembers(ctx context.Context, st *store.Store, bundle *enrollmentBundle) (uint64, string, store.ClusterIdentity, []store.ClusterTransportMember, error) {
	var expected store.ClusterIdentity
	if st == nil || bundle == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.trust == nil {
		return 0, "", expected, nil, errors.New("material replication requires a local member identity")
	}
	route, ok := st.ClusterSessionRoute()
	if !ok {
		return 0, "", expected, nil, errors.New("material replication requires the local live member")
	}
	leaf := bundle.tls.Certificates[0].Leaf
	if leaf == nil {
		return 0, "", expected, nil, errors.New("material replication requires a parsed local certificate")
	}
	localKey, err := bundle.trust.VerifyMember(leaf, time.Now().UTC())
	if err != nil {
		return 0, "", expected, nil, err
	}
	expected = store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	_, members, err := st.GrantClusterMembership(ctx, expected, localKey)
	if err != nil {
		return 0, "", expected, nil, err
	}
	self := false
	for _, member := range members {
		if member.ID == route.ID {
			self = member.KeyFingerprint == localKey
			break
		}
	}
	if !self {
		return 0, "", expected, nil, errors.New("local material sender differs from live membership")
	}
	return route.ID, localKey, expected, members, nil
}

// A source must publish its own HTTPS origin through the authenticated member
// route before another member can use it for a pinned fresh-byte read.
func registerOwnMaterialOrigin(ctx context.Context, bundle *enrollmentBundle, localKey string) error {
	if bundle == nil || bundle.dataDir == "" || bundle.info.PublicURL == "" || localKey == "" {
		return errors.New("material source has no local HTTPS identity")
	}
	client, err := enrollment.NewClient(bundle.info.PublicURL+"/api/v1/cluster/enrollment/certificate", bundle.info.ClusterID, bundle.info.CAFingerprint, bundle.info.Protocol)
	if err != nil {
		return err
	}
	defer client.Close()
	certificate, err := os.ReadFile(filepath.Join(bundle.dataDir, enrollmentDirectory, "server.pem"))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(bundle.dataDir, enrollmentDirectory, "server.key"))
	if err != nil {
		return err
	}
	return client.RegisterClusterEnrollmentOrigin(ctx, certificate, key, bundle.info.PublicURL, localKey)
}

// ensureRemoteMaterialReplica uses a receipt only as a hint. A fresh pinned
// probe proves the current receiver bytes before the expensive transfer is
// skipped. A failed probe falls back to a full transfer and readback.
func ensureRemoteMaterialReplica(ctx context.Context, st *store.Store, client *http.Client, origin, source, digest string, size int64, memberID uint64, onTransfer func()) (bool, error) {
	hasReceipt, err := st.HasMaterialRemoteReceipt(ctx, digest, memberID, size)
	if err != nil {
		return false, err
	}
	if hasReceipt {
		probeErr := probeRemoteMaterialReplica(ctx, client, origin, digest, size, memberID)
		if probeErr == nil {
			return false, nil
		}
		if peerUnreachable(probeErr) {
			// The peer answered nothing about the bytes it holds. Sending them
			// again would report a change that did not happen.
			return false, probeErr
		}
	}
	ack, err := sendMaterialReplica(ctx, client, origin, source, digest, memberID)
	if err != nil {
		return false, err
	}
	if onTransfer != nil {
		onTransfer()
	}
	if ack.Bytes != size {
		return false, errors.New("material receipt does not match inventory size")
	}
	if err := verifyRemoteMaterialReplica(ctx, client, origin, digest, size, memberID); err != nil {
		return false, err
	}
	return true, nil
}

func persistLocalMaterialBytes(ctx context.Context, dataDir string, st *store.Store, memberID uint64, digest string, body []byte) (string, error) {
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root = filepath.Join(root, "material-replicas")
	receiver, err := newMaterialReceiver(root, memberID)
	if err != nil {
		return "", err
	}
	if _, err := receiver.receive(ctx, digest, bytes.NewReader(body)); err != nil {
		return "", err
	}
	path := filepath.Join(root, digest[:2], digest)
	if err := st.RecordLocalMaterialReplicaReference(ctx, digest, memberID, path); err != nil {
		return "", err
	}
	return path, nil
}

// persistLocalMaterialInventory makes a private, immutable source independent
// of later edits to the original path before publishing its Store reference.
func persistLocalMaterialInventory(ctx context.Context, dataDir string, st *store.Store, memberID uint64, inventory MaterialInventory) (map[string]string, error) {
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root = filepath.Join(root, "material-replicas")
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := syncMaterialDir(filepath.Dir(root)); err != nil {
		return nil, err
	}
	receiver, err := newMaterialReceiver(root, memberID)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(inventory.Files))
	for _, material := range inventory.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, exists := paths[material.SHA256]; exists {
			continue
		}
		path := filepath.Join(root, material.SHA256[:2], material.SHA256)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			source, err := os.Open(material.Path)
			if err != nil {
				return nil, err
			}
			_, receivedErr := receiver.receive(ctx, material.SHA256, source)
			closeErr := source.Close()
			if err := errors.Join(receivedErr, closeErr); err != nil {
				return nil, fmt.Errorf("persist %s material locally: %w", material.Class, err)
			}
		} else if err != nil {
			return nil, err
		}
		paths[material.SHA256] = path
	}
	if err := st.RecordLocalMaterialReplicaReferences(ctx, memberID, paths); err != nil {
		return nil, fmt.Errorf("index material inventory locally: %w", err)
	}
	return paths, nil
}

// A successful full pass gives new local references a five-tick upper bound.
// A shorter remote verification pass finds lost copies without rescanning the
// source or reinstalling unchanged consumer paths on every tick.
func materialReplicationDue(now time.Time, interval time.Duration, lastFull, lastProbe time.Time, sameMembers bool) (full, probe bool) {
	if !sameMembers || lastFull.IsZero() || now.Before(lastFull) || now.Sub(lastFull) >= 5*interval {
		return true, false
	}
	if lastProbe.IsZero() || now.Before(lastProbe) || now.Sub(lastProbe) >= 2*interval {
		return false, true
	}
	return false, false
}

// This is a repair probe, not a readiness decision. The receiver freshly
// checks all manifest bytes against the live source identity; a failure
// triggers a complete source scan and transfer on this same worker iteration.
func probePublishedMaterialManifest(ctx context.Context, st *store.Store, bundle *enrollmentBundle, sourceID uint64, sourceKey string, expected store.ClusterIdentity, members []store.ClusterTransportMember) error {
	if len(members) < 2 {
		return nil
	}
	pointer, err := st.MaterialManifestBySource(ctx, int64(sourceID))
	if err != nil {
		return err
	}
	if pointer.ClusterID != expected.ClusterID || pointer.SourceKey != sourceKey || !validMaterialDigest(pointer.Digest) || pointer.Bytes <= 0 {
		return errors.New("published material manifest differs from current sender")
	}
	for _, member := range members {
		if member.ID == sourceID {
			continue
		}
		client, err := memberMaterialClient(bundle, member.EnrollmentOrigin, member.KeyFingerprint)
		if err != nil {
			return err
		}
		err = confirmRemoteMaterialManifest(ctx, client, member.EnrollmentOrigin, pointer.Digest, sourceID, member.ID)
		client.CloseIdleConnections()
		if err != nil {
			// A partitioned member's refusal must not close this member; the
			// full run that follows skips it like any unavailable peer.
			return fmt.Errorf("probe material member %d: %w", member.ID, explainMemberReachability(ctx, err, member.Address))
		}
	}
	_, current, err := st.GrantClusterMembership(ctx, expected, sourceKey)
	if err != nil {
		return err
	}
	if !slices.Equal(current, members) {
		return errors.New("material membership changed during remote probe")
	}
	return nil
}

func (a *App) runMaterialReplication(ctx context.Context, interval time.Duration) {
	if a.enrollment == nil {
		return
	}
	requests := a.enrollment.materialRequests
	if requests == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastFull, lastProbe, lastInstall time.Time
	var lastInstalledDigest string
	var lastObservedMembers []store.ClusterTransportMember
	for {
		err := requests.run(ctx, func(workCtx context.Context) error {
			bundle, err := readEnrollmentBundle(a.cfg.DataDir, false)
			if err != nil {
				return err
			}
			var startupRejected error
			sourceID, sourceKey, expected, members, err := materialSenderMembers(workCtx, a.store, bundle)
			if err == nil {
				for _, member := range members {
					if member.ID == sourceID && member.EnrollmentOrigin != bundle.info.PublicURL {
						a.invalidateCandidateMaterialEligibility()
						err = registerOwnMaterialOrigin(workCtx, bundle, sourceKey)
						if err == nil {
							sourceID, sourceKey, expected, members, err = materialSenderMembers(workCtx, a.store, bundle)
						}
						break
					}
				}
			}
			if err == nil {
				if !slices.ContainsFunc(members, func(member store.ClusterTransportMember) bool {
					return member.ID == sourceID && member.EnrollmentOrigin == bundle.info.PublicURL
				}) {
					return errors.New("local material origin did not stabilize after registration")
				}
				sameMembers := slices.Equal(lastObservedMembers, members)
				if lastFull.IsZero() && lastInstalledDigest == "" {
					pointer, restartErr := checkMaterialReplicationRestart(workCtx, a.cfg.DataDir, a.store, bundle, sourceID, sourceKey, expected, members)
					var canceled bool
					startupRejected, canceled = a.materialStartupFailure(workCtx, restartErr)
					if canceled {
						return startupRejected
					}
					if restartErr == nil {
						if len(lastObservedMembers) > 0 && !sameMembers && !observedMembersOnlyShrink(lastObservedMembers, members) && localMaterialMemberAbsent(sourceID, bundle.info.PublicURL, members) {
							a.invalidateCandidateMaterialEligibility()
						}
						lastInstalledDigest = pointer.Digest
						lastFull = time.Now()
						lastProbe, lastInstall = lastFull, lastFull
						sameMembers = true
						if a.log != nil {
							a.log.Debug("cluster material restart scheduling restored", "source_member", sourceID, "members", len(members))
						}
					} else if a.log != nil {
						a.log.Debug("cluster material restart requires full replication", "source_member", sourceID, "err", restartErr)
					}
				}
				// A peer restart or a vanished voter changes the live member
				// list without removing this member's admission. Close only when
				// this process itself is gone from the list; that also covers a
				// list that merely shrank around this member.
				if !sameMembers && localMaterialMemberAbsent(sourceID, bundle.info.PublicURL, members) {
					a.invalidateCandidateMaterialEligibility()
				}
				// Observation is independent of repair success. These members
				// schedule work only; grants and final checks still bind each run.
				lastObservedMembers = slices.Clone(members)
				fullRun := func() error {
					installDue := materialInstallationDue(time.Now(), lastInstall, interval, sameMembers)
					if err := a.replicateMaterialCycle(workCtx, bundle, sourceID, sourceKey, expected, members, lastInstalledDigest, sameMembers, installDue); err != nil {
						lastFull = time.Time{}
						lastInstall = time.Time{}
						return err
					}
					_, current, err := a.store.GrantClusterMembership(workCtx, expected, sourceKey)
					if err != nil || !slices.Equal(current, members) {
						lastFull = time.Time{}
						lastInstall = time.Time{}
						if err != nil {
							return err
						}
						return errors.New("material membership changed during full replication")
					}
					if len(members) >= 2 {
						pointer, err := a.store.MaterialManifestBySource(workCtx, int64(sourceID))
						if err != nil {
							lastFull = time.Time{}
							lastInstall = time.Time{}
							return err
						}
						if installDue || pointer.Digest != lastInstalledDigest {
							lastInstall = time.Now()
						}
						lastInstalledDigest = pointer.Digest
					} else {
						lastInstall = time.Time{}
						lastInstalledDigest = ""
					}
					lastFull = time.Now()
					lastProbe = lastFull
					return nil
				}
				full, probe := materialReplicationDue(time.Now(), interval, lastFull, lastProbe, sameMembers)
				switch {
				case full:
					err = fullRun()
				case probe:
					err = probePublishedMaterialManifest(workCtx, a.store, bundle, sourceID, sourceKey, expected, members)
					if err != nil {
						if materialReplicationFailureInvalidates(err) {
							a.invalidateCandidateMaterialEligibility()
						}
						err = fullRun()
					} else {
						lastProbe = time.Now()
					}
				}
			}
			return materialStartupResult(startupRejected, err)
		})
		if err != nil && ctx.Err() == nil {
			a.materialReplicationPassFailed(err)
		} else if err == nil {
			a.materialReplicationPassCompleted()
		}
		if !waitForMaterialReplicationCycle(ctx, ticker.C, interval, err != nil) {
			return
		}
	}
}

func (a *App) yieldWarmMaterialBatch(ctx context.Context) error {
	a.materialEligibilityMu.Lock()
	eligibility := a.materialEligibility
	a.materialEligibilityMu.Unlock()
	return eligibility.waitForWorkerProof(ctx, store.ClusterMembershipGrantTimeout)
}

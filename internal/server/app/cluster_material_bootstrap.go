//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// CheckClusterMaterialBootstrap checks the installing member's own published
// inventory on both other members before formal readiness is tested. read
// must finish its read-only native snapshot and cleanup before returning.
// These observations never replace the application's material eligibility.
func CheckClusterMaterialBootstrap(ctx context.Context, cfg Config, ownHost string, ownID uint64, hosts []string, databasePort, enrollmentPort int, read func(context.Context, store.ClusterIdentity, string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		return err
	}
	return checkClusterMaterialBootstrap(ctx, bundle, ownHost, ownID, hosts, databasePort, enrollmentPort, read)
}

func checkClusterMaterialBootstrap(ctx context.Context, bundle *enrollmentBundle, ownHost string, ownID uint64, hosts []string, databasePort, enrollmentPort int, read func(context.Context, store.ClusterIdentity, string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil || read == nil || ownID == 0 || len(hosts) != 3 || databasePort < 1 || databasePort > 65535 || enrollmentPort < 1 || enrollmentPort > 65535 {
		return errors.New("bootstrap material verification requires the installation's exact member identity")
	}
	allowed := make(map[string]string, len(hosts))
	for _, host := range hosts {
		if net.ParseIP(host) == nil || allowed[net.JoinHostPort(host, strconv.Itoa(databasePort))] != "" {
			return errors.New("bootstrap material installation hosts are invalid or duplicated")
		}
		allowed[net.JoinHostPort(host, strconv.Itoa(databasePort))] = "https://" + net.JoinHostPort(host, strconv.Itoa(enrollmentPort))
	}
	ownAddress := net.JoinHostPort(ownHost, strconv.Itoa(databasePort))
	if allowed[ownAddress] == "" || bundle.info.PublicURL != allowed[ownAddress] {
		return errors.New("bootstrap material source origin differs from the installation")
	}
	key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return err
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	phase, members, pointers, err := read(ctx, expected, key)
	if err != nil {
		return err
	}
	if phase.Phase != store.MaintenanceServing || phase.Revision <= 0 || len(members) != 3 || len(pointers) != 3 {
		return errors.New("bootstrap material snapshot is not complete and serving")
	}
	seen := make(map[string]bool, len(members))
	seenIDs := make(map[uint64]bool, len(members))
	self := false
	var ownPointer store.MaterialManifestRecord
	for _, member := range members {
		if member.ID == 0 || member.Role != "voter" || allowed[member.Address] == "" || member.EnrollmentOrigin != allowed[member.Address] || seen[member.Address] || seenIDs[member.ID] {
			return errors.New("bootstrap material member differs from the installation")
		}
		seen[member.Address], seenIDs[member.ID] = true, true
		if member.ID == ownID {
			self = member.KeyFingerprint == key && member.Address == ownAddress
		}
		found := false
		for _, pointer := range pointers {
			if uint64(pointer.SourceMemberID) != member.ID {
				continue
			}
			if found || pointer.ClusterID != expected.ClusterID || pointer.SourceKey != member.KeyFingerprint || !validMaterialDigest(pointer.Digest) || pointer.Bytes <= 0 || pointer.Bytes > maxMaterialManifestBytes {
				return errors.New("bootstrap material pointer differs from its live source")
			}
			found = true
			if member.ID == ownID {
				ownPointer = pointer
			}
		}
		if !found {
			return errors.New("bootstrap material source manifest is missing")
		}
	}
	if !self {
		return errors.New("bootstrap material source is not the installing live member")
	}
	for _, member := range members {
		if member.ID == ownID {
			continue
		}
		client, err := memberMaterialClient(bundle, member.EnrollmentOrigin, member.KeyFingerprint)
		if err != nil {
			return err
		}
		err = checkRemoteInstalledMaterialManifest(ctx, client, member.EnrollmentOrigin, ownPointer.Digest, ownID, member.ID)
		client.CloseIdleConnections()
		if err != nil {
			return err
		}
	}
	// No read transaction survives the installed HTTP calls. Obtain a new
	// closed snapshot; authorization counters legitimately advanced meanwhile.
	afterPhase, afterMembers, afterPointers, err := read(ctx, expected, key)
	if err != nil {
		return err
	}
	if afterPhase != phase || !slices.Equal(afterMembers, members) || !sameBootstrapMaterialPointers(pointers, afterPointers) {
		return errors.New("bootstrap material phase, members, or pointers changed during installed verification")
	}
	return ctx.Err()
}

func sameBootstrapMaterialPointers(before, after []store.MaterialManifestRecord) bool {
	if len(before) != len(after) {
		return false
	}
	for i, pointer := range before {
		current := after[i]
		if pointer.SourceMemberID != current.SourceMemberID || pointer.ClusterID != current.ClusterID || pointer.SourceKey != current.SourceKey || pointer.Digest != current.Digest || pointer.Bytes != current.Bytes || !pointer.RecordedAt.Equal(current.RecordedAt) {
			return false
		}
	}
	return true
}

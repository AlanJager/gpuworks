//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// verifyReceivedMaterialManifest consumes a published manifest on the actual
// receiving member. A replicated pointer or receipt cannot stand in for the
// receiver's own current bytes. This does not install consumer paths or grant
// application readiness.
func verifyReceivedMaterialManifest(ctx context.Context, st *store.Store, receiver *materialReceiver, expected store.ClusterIdentity, sourceKey, digest string) (materialManifest, error) {
	if st == nil || receiver == nil || !validMaterialDigest(digest) {
		return materialManifest{}, errors.New("material manifest verification requires a receiver and digest")
	}
	_, members, err := grantMaterialProofMembership(ctx, st, expected, sourceKey)
	if err != nil {
		return materialManifest{}, err
	}
	var sourceID uint64
	self := false
	for _, member := range members {
		if member.KeyFingerprint == sourceKey {
			sourceID = member.ID
		}
		if member.ID == receiver.memberID {
			self = true
		}
	}
	if sourceID == 0 || sourceID == receiver.memberID || !self {
		return materialManifest{}, errors.New("material manifest source or receiver is not a live member")
	}
	pointerStarted := candidateProofStart(ctx)
	pointer, pointerErr := st.MaterialManifestBySource(ctx, int64(sourceID))
	candidateProofObserve(ctx, proofPointerCheck, pointerStarted, 0)
	if pointerErr != nil {
		return materialManifest{}, pointerErr
	}
	if pointer.ClusterID != expected.ClusterID || pointer.SourceKey != sourceKey || pointer.Digest != digest || pointer.Bytes <= 0 || pointer.Bytes > maxMaterialManifestBytes {
		return materialManifest{}, errors.New("material manifest pointer differs from current source")
	}
	manifest, err := verifyReceivedMaterialManifestBytes(ctx, st, receiver, expected, pointer)
	if err != nil {
		return materialManifest{}, err
	}
	_, current, err := grantMaterialProofMembership(ctx, st, expected, sourceKey)
	if err != nil {
		return materialManifest{}, err
	}
	if !slices.ContainsFunc(current, func(member store.ClusterTransportMember) bool {
		return member.ID == sourceID && member.KeyFingerprint == sourceKey
	}) ||
		!slices.ContainsFunc(current, func(member store.ClusterTransportMember) bool { return member.ID == receiver.memberID }) {
		return materialManifest{}, errors.New("material manifest membership changed during verification")
	}
	pointerStarted = candidateProofStart(ctx)
	latest, pointerErr := st.MaterialManifestBySource(ctx, int64(sourceID))
	candidateProofObserve(ctx, proofPointerCheck, pointerStarted, 0)
	if pointerErr != nil {
		// Keep the cause: a read the database interrupted is not a changed pointer.
		return materialManifest{}, fmt.Errorf("material manifest pointer reread: %w", pointerErr)
	}
	if latest != pointer {
		return materialManifest{}, errors.New("material manifest pointer changed during verification")
	}
	return manifest, nil
}

// verifyReceivedMaterialManifestBytes checks canonical bytes, both index reads,
// and the installed master against an already bound current source pointer.
// It does not grant authority: callers must bracket it with fresh grants and
// recheck the complete pointer after their final grant.
func verifyReceivedMaterialManifestBytes(ctx context.Context, st *store.Store, receiver *materialReceiver, expected store.ClusterIdentity, pointer store.MaterialManifestRecord) (materialManifest, error) {
	if st == nil || receiver == nil || pointer.SourceMemberID == 0 || uint64(pointer.SourceMemberID) == receiver.memberID || pointer.ClusterID != expected.ClusterID || !validMaterialDigest(pointer.Digest) || pointer.Bytes <= 0 || pointer.Bytes > maxMaterialManifestBytes {
		return materialManifest{}, errors.New("material manifest verification requires a bound current source pointer")
	}
	digest, sourceID, sourceKey := pointer.Digest, uint64(pointer.SourceMemberID), pointer.SourceKey
	manifestPath := filepath.Join(receiver.root, digest[:2], digest)
	if err := checkReceivedMaterialRow(ctx, st, receiver, digest, manifestPath); err != nil {
		return materialManifest{}, err
	}
	raw, err := readReceivedMaterial(ctx, manifestPath, digest, pointer.Bytes)
	if err != nil {
		return materialManifest{}, err
	}
	manifest, err := parseMaterialManifest(raw, expected, sourceID, sourceKey)
	if err != nil {
		return materialManifest{}, err
	}
	digests := make([]string, 0, len(manifest.Files))
	verified := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		if !verified[file.SHA256] {
			verified[file.SHA256] = true
			digests = append(digests, file.SHA256)
		}
	}
	if err := checkReceivedMaterialRows(ctx, st, receiver, digests); err != nil {
		return materialManifest{}, fmt.Errorf("manifest material is not indexed locally: %w", err)
	}
	clear(verified)
	masterSeen := false
	for _, file := range manifest.Files {
		if file.Class == MaterialMasterSecret && file.Path == "master.key" {
			masterSeen = true
			if err := verifyReceivedMaterial(ctx, filepath.Join(filepath.Dir(receiver.root), file.Path), file.SHA256, file.Bytes); err != nil {
				return materialManifest{}, fmt.Errorf("installed master secret differs from the source manifest: %w", err)
			}
		}
		if verified[file.SHA256] {
			continue
		}
		path := filepath.Join(receiver.root, file.SHA256[:2], file.SHA256)
		if err := verifyReceivedMaterial(ctx, path, file.SHA256, file.Bytes); err != nil {
			return materialManifest{}, fmt.Errorf("%s material differs locally: %w", file.Class, err)
		}
		verified[file.SHA256] = true
	}
	if !masterSeen {
		return materialManifest{}, errors.New("material manifest omits the installed master secret")
	}
	// Recheck the index after reading all bytes. A row removed or redirected
	// during a batch cannot be accepted from the earlier query alone.
	if err := checkReceivedMaterialRows(ctx, st, receiver, digests); err != nil {
		return materialManifest{}, fmt.Errorf("manifest material index changed during verification: %w", err)
	}
	return manifest, nil
}

func checkReceivedMaterialRow(ctx context.Context, st *store.Store, receiver *materialReceiver, digest, path string) error {
	row, err := st.MaterialReplicaByDigest(ctx, digest, int64(receiver.memberID))
	if err != nil {
		return err
	}
	if row.Path != path {
		return errors.New("material replica path is not the receiver's canonical path")
	}
	return nil
}

func checkReceivedMaterialRows(ctx context.Context, st *store.Store, receiver *materialReceiver, digests []string) error {
	const batchSize = 64
	for start := 0; start < len(digests); start += batchSize {
		batch := digests[start:min(start+batchSize, len(digests))]
		rows, err := st.MaterialReplicasByDigests(ctx, int64(receiver.memberID), batch)
		if err != nil {
			return err
		}
		if len(rows) != len(batch) {
			return errors.New("material replica index is incomplete")
		}
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			if !validMaterialDigest(row.Digest) || seen[row.Digest] ||
				row.Path != filepath.Join(receiver.root, row.Digest[:2], row.Digest) {
				return errors.New("material replica index has a noncanonical or duplicate row")
			}
			seen[row.Digest] = true
		}
		for _, digest := range batch {
			if !seen[digest] {
				return errors.New("material replica index is missing an expected digest")
			}
		}
	}
	return nil
}

func readReceivedMaterial(ctx context.Context, path, digest string, size int64) ([]byte, error) {
	started := candidateProofStart(ctx)
	verifiedBytes := int64(0)
	defer func() { candidateProofObserve(ctx, proofLocalHash, started, verifiedBytes) }()
	if size <= 0 || size > maxMaterialManifestBytes {
		return nil, errors.New("material manifest length is invalid")
	}
	file, initial, err := openReceivedMaterial(path, size)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(raw)) != size {
		return nil, errors.New("material manifest bytes are incomplete")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest || ctx.Err() != nil {
		return nil, errors.New("material manifest digest differs or verification canceled")
	}
	if err := checkReceivedMaterialIdentity(path, initial); err != nil {
		return raw, err
	}
	verifiedBytes = int64(len(raw))
	return raw, nil
}

func verifyReceivedMaterial(ctx context.Context, path, digest string, size int64) error {
	started := candidateProofStart(ctx)
	verifiedBytes := int64(0)
	defer func() { candidateProofObserve(ctx, proofLocalHash, started, verifiedBytes) }()
	file, initial, err := openReceivedMaterial(path, size)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			count += int64(n)
			_, _ = hash.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil || count > size {
			return errors.New("received material bytes are incomplete")
		}
		if n == 0 {
			return errors.New("received material reader made no progress")
		}
	}
	if count != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("received material digest differs")
	}
	if err := checkReceivedMaterialIdentity(path, initial); err != nil {
		return err
	}
	verifiedBytes = count
	return nil
}

func openReceivedMaterial(path string, size int64) (*os.File, os.FileInfo, error) {
	initial, err := os.Lstat(path)
	if err != nil || !initial.Mode().IsRegular() || initial.Size() != size {
		return nil, nil, errors.New("received material file is missing or differs")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) {
		_ = file.Close()
		return nil, nil, errors.New("received material file changed before reading")
	}
	return file, opened, nil
}

func checkReceivedMaterialIdentity(path string, opened os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || current.Size() != opened.Size() {
		return errors.New("received material file changed after reading")
	}
	return nil
}

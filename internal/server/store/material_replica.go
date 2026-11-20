package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaterialReplica is one member's durable copy of a required file digest.
// Readiness needs two readable copies so one fault still leaves the bytes.
type MaterialReplica struct {
	Digest string `gorm:"primaryKey;size:64"`
	// SQLite stores the full uint64 Raft ID bit pattern as a signed INTEGER.
	MemberID   int64  `gorm:"primaryKey"`
	Path       string `gorm:"size:512;not null"`
	RecordedAt time.Time
}

// MaterialRemoteReceipt records the receiver's authenticated acknowledgement
// for a content-addressed file on another member. The key (digest, member ID)
// and byte length identify the receipt; a replicated row alone never proves
// that the remote bytes remain readable.
type MaterialRemoteReceipt struct {
	Digest string `gorm:"primaryKey;size:64"`
	// Keep the uint64 Raft ID bit pattern in SQLite's signed INTEGER range.
	MemberID   int64 `gorm:"primaryKey"`
	Bytes      int64 `gorm:"not null"`
	RecordedAt time.Time
}

// RecordMaterialRemoteReceipt must only be called after a pinned, mutually
// authenticated transfer returned this exact digest, member ID, and byte
// length. It never accepts a caller-supplied path as evidence of remote bytes.
// Replaying an identical receipt is harmless; contradictory lengths fail. A
// same-value replay is not a fresh majority authorization for remote bytes.
func (s *Store) RecordMaterialRemoteReceipt(ctx context.Context, digest string, memberID uint64, size int64) error {
	if !materialDigest(digest) || memberID == 0 || size <= 0 {
		return errors.New("remote material receipt requires a digest, member, and positive size")
	}
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		row := MaterialRemoteReceipt{Digest: digest, MemberID: int64(memberID), Bytes: size, RecordedAt: time.Now().UTC()}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var existing MaterialRemoteReceipt
		if err := tx.Where("digest = ? AND member_id = ?", digest, int64(memberID)).First(&existing).Error; err != nil {
			return err
		}
		if existing.Bytes != size {
			return errors.New("remote material receipt contradicts recorded size")
		}
		return nil
	})
}

// HasMaterialRemoteReceipt identifies a prior transfer for this exact member
// and byte length. Callers must still probe the member's current bytes before
// skipping a transfer; the row alone is not a readiness signal.
func (s *Store) HasMaterialRemoteReceipt(ctx context.Context, digest string, memberID uint64, size int64) (bool, error) {
	if !materialDigest(digest) || memberID == 0 || size <= 0 {
		return false, errors.New("remote material receipt lookup requires a digest, member, and positive size")
	}
	var row MaterialRemoteReceipt
	err := s.db.WithContext(ctx).Where("digest = ? AND member_id = ?", digest, int64(memberID)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Bytes != size {
		return false, errors.New("remote material receipt contradicts expected size")
	}
	return true, nil
}

// MaterialRemoteVerifier checks a receiver's current bytes through an
// authenticated member endpoint. It must bind all three receipt fields and
// return an error when the bytes cannot be read or authenticated.
type MaterialRemoteVerifier func(context.Context, MaterialRemoteReceipt) error

// MaterialSurvivesMemberFaultWithVerifiedRemotes checks this member's local
// bytes and freshly probes each remote receipt. A DB receipt or a local path
// labelled with another member's ID cannot count as a remote copy. The caller
// must supply its actual local member ID and a pinned remote verifier.
func (s *Store) MaterialSurvivesMemberFaultWithVerifiedRemotes(ctx context.Context, digest string, localMemberID uint64, verify MaterialRemoteVerifier) (bool, error) {
	if !materialDigest(digest) || localMemberID == 0 || verify == nil {
		return false, errors.New("material verification requires a digest, local member, and remote verifier")
	}
	var local MaterialReplica
	localErr := s.db.WithContext(ctx).Where("digest = ? AND member_id = ?", digest, int64(localMemberID)).First(&local).Error
	if localErr != nil && !errors.Is(localErr, gorm.ErrRecordNotFound) {
		return false, localErr
	}
	localReadable := false
	if localErr == nil {
		_, localReadable = readableMaterialID(ctx, local.Path, digest)
	}
	var remote []MaterialRemoteReceipt
	if err := s.db.WithContext(ctx).Where("digest = ? AND member_id <> ?", digest, int64(localMemberID)).Find(&remote).Error; err != nil {
		return false, err
	}
	verified := 0
	for _, receipt := range remote {
		if receipt.Bytes <= 0 {
			continue
		}
		if err := verify(ctx, receipt); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			continue
		}
		verified++
	}
	if localReadable {
		// Re-read after remote probes so a removed local file does not pass.
		_, localReadable = readableMaterialID(ctx, local.Path, digest)
	}
	if localReadable {
		verified++
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return verified >= 2, nil
}

// RecordMaterialReplica copies source bytes onto copyPath for one member.
// A digest row without those bytes is not a replica. If indexing is denied,
// leave the unindexed file in place: copyPath may predate this call or be
// owned by another user of the same bytes.
func (s *Store) RecordMaterialReplica(ctx context.Context, digest string, memberID uint64, source, copyPath string) error {
	if !materialDigest(digest) || memberID == 0 || source == "" || copyPath == "" {
		return errors.New("material replica requires a digest, a member, and a file")
	}
	body, err := os.ReadFile(source)
	if err != nil || len(body) == 0 {
		return errors.New("material replica requires readable bytes")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return errors.New("material replica digest does not match the file")
	}
	if err := os.MkdirAll(filepath.Dir(copyPath), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(copyPath, body, 0600); err != nil {
		return err
	}
	written, err := os.ReadFile(copyPath)
	if err != nil || len(written) != len(body) {
		return errors.New("material replica copy is not readable")
	}
	row := MaterialReplica{Digest: digest, MemberID: int64(memberID), Path: copyPath, RecordedAt: time.Now().UTC()}
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "digest"}, {Name: "member_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"path", "recorded_at"}),
		}).Create(&row).Error
	})
}

// RecordLocalMaterialReplicaReference indexes an already durable local file.
// Callers must fsync the immutable content-addressed file and its directories
// before calling; this method rehashes the named file and never copies bytes.
func (s *Store) RecordLocalMaterialReplicaReference(ctx context.Context, digest string, memberID uint64, path string) error {
	if !materialDigest(digest) || memberID == 0 || path == "" {
		return errors.New("local material reference requires a digest, member, and file")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := readableMaterialID(ctx, path, digest); !ok {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("local material reference is missing or differs")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	row := MaterialReplica{Digest: digest, MemberID: int64(memberID), Path: path, RecordedAt: time.Now().UTC()}
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "digest"}, {Name: "member_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"path", "recorded_at"}),
		}).Create(&row).Error
	})
}

// RecordLocalMaterialReplicaReferences indexes one member's already durable
// content-addressed files in one serving-phase transaction. Every file is
// rehashed before the transaction; no partial index is committed if any
// source is missing, changed, or the serving phase has closed.
func (s *Store) RecordLocalMaterialReplicaReferences(ctx context.Context, memberID uint64, paths map[string]string) error {
	if memberID == 0 {
		return errors.New("local material references require a member")
	}
	if len(paths) == 0 {
		return nil
	}
	digests := make([]string, 0, len(paths))
	for digest := range paths {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	rows := make([]MaterialReplica, 0, len(digests))
	now := time.Now().UTC()
	for _, digest := range digests {
		path := paths[digest]
		if !materialDigest(digest) || path == "" {
			return errors.New("local material reference requires a digest and file")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := readableMaterialID(ctx, path, digest); !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("local material reference is missing or differs")
		}
		rows = append(rows, MaterialReplica{Digest: digest, MemberID: int64(memberID), Path: path, RecordedAt: now})
	}
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "digest"}, {Name: "member_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"path", "recorded_at"}),
			// Identical paths were fully rehashed above; indexing them again
			// does not establish new authority or refresh their recorded time.
			Where: clause.Where{Exprs: []clause.Expression{
				clause.Neq{Column: clause.Column{Table: "material_replicas", Name: "path"}, Value: clause.Column{Table: "excluded", Name: "path"}},
			}},
		}).CreateInBatches(rows, 64).Error
	})
}

// materialCopy is one member's readable file. id identifies the underlying
// file so two rows that share one inode are not two copies.
type materialCopy struct {
	id   string
	path string
}

// MaterialSurvivesMemberFault checks legacy, same-process local path fixtures.
// It cannot establish cross-host readiness: use the authenticated remote
// receipt and live probe method above for actual cluster members.
func (s *Store) MaterialSurvivesMemberFault(ctx context.Context, digest string) (bool, error) {
	if !materialDigest(digest) {
		return false, errors.New("material replica requires a digest and a member")
	}
	var rows []MaterialReplica
	if err := s.db.WithContext(ctx).Where("digest = ?", digest).Find(&rows).Error; err != nil {
		return false, err
	}
	copies := map[uint64][]materialCopy{}
	for _, row := range rows {
		id, ok := readableMaterialID(ctx, row.Path, digest)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		memberID := uint64(row.MemberID)
		copies[memberID] = append(copies[memberID], materialCopy{id: id, path: row.Path})
	}
	if len(copies) < 2 {
		return false, nil
	}
	for drop, removed := range copies {
		gone := map[string]struct{}{}
		for _, copy := range removed {
			gone[copy.id] = struct{}{}
		}
		survives := false
		for member, files := range copies {
			if member == drop {
				continue
			}
			for _, copy := range files {
				if _, shared := gone[copy.id]; shared {
					continue
				}
				if _, ok := readableMaterialID(ctx, copy.path, digest); !ok {
					if err := ctx.Err(); err != nil {
						return false, err
					}
					continue
				}
				survives = true
			}
		}
		if !survives {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

func readableMaterialID(ctx context.Context, path, digest string) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	initial, err := os.Lstat(path)
	if err != nil || !initial.Mode().IsRegular() || initial.Size() <= 0 {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(initial, info) {
		return "", false
	}
	hash := sha256.New()
	count, err := io.CopyBuffer(hash, materialStoreContextReader{ctx: ctx, reader: file}, make([]byte, 128<<10))
	if err != nil || ctx.Err() != nil || count != info.Size() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", false
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return filepath.Clean(path), true
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(uint64(stat.Ino), 10), true
}

type materialStoreContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader materialStoreContextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(p)
}

func materialDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	return strings.Trim(digest, "0123456789abcdef") == ""
}

func migrateMaterialReplicas(db *gorm.DB) error {
	return db.AutoMigrate(&MaterialReplica{})
}

func migrateMaterialRemoteReceipts(db *gorm.DB) error {
	return db.AutoMigrate(&MaterialRemoteReceipt{})
}

package store

import "context"

// The readers below list the rows whose files a cluster member must hold.
// They select only the columns that name those files.

// OfflineImageBlobRows returns images that carry an offline blob.
func (s *Store) OfflineImageBlobRows(ctx context.Context) ([]Image, error) {
	var rows []Image
	err := s.db.WithContext(ctx).Model(&Image{}).Select("offline_blob_path", "offline_blob_sha256", "offline_blob_bytes").Where("offline_blob_path <> ''").Find(&rows).Error
	return rows, err
}

// BenchmarkTokenizerRows returns benchmark runs that name a tokenizer file.
func (s *Store) BenchmarkTokenizerRows(ctx context.Context) ([]BenchmarkRun, error) {
	var rows []BenchmarkRun
	err := s.db.WithContext(ctx).Model(&BenchmarkRun{}).Select("tokenizer_ref").Where("tokenizer_ref <> ''").Find(&rows).Error
	return rows, err
}

// BenchmarkDetailRows returns benchmark points that name a detail file.
func (s *Store) BenchmarkDetailRows(ctx context.Context) ([]BenchmarkPoint, error) {
	var rows []BenchmarkPoint
	err := s.db.WithContext(ctx).Model(&BenchmarkPoint{}).Select("detail_path").Where("detail_path <> ''").Find(&rows).Error
	return rows, err
}

// ModelLogoRows returns models that name a logo file.
func (s *Store) ModelLogoRows(ctx context.Context) ([]Model, error) {
	var rows []Model
	err := s.db.WithContext(ctx).Model(&Model{}).Select("logo_path").Where("logo_path <> ''").Find(&rows).Error
	return rows, err
}

// CountMaterialReplicas counts the receipts one member holds for a digest.
func (s *Store) CountMaterialReplicas(ctx context.Context, digest string, memberID int64) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&MaterialReplica{}).Where("digest = ? AND member_id = ?", digest, memberID).Count(&n).Error
	return n, err
}

// MaterialReplicaByDigest returns one member's receipt for a digest.
// A missing row is gorm.ErrRecordNotFound.
func (s *Store) MaterialReplicaByDigest(ctx context.Context, digest string, memberID int64) (MaterialReplica, error) {
	var row MaterialReplica
	err := s.db.WithContext(ctx).Where("digest = ? AND member_id = ?", digest, memberID).First(&row).Error
	return row, err
}

// MaterialReplicasByDigests returns one member's receipts among digests.
func (s *Store) MaterialReplicasByDigests(ctx context.Context, memberID int64, digests []string) ([]MaterialReplica, error) {
	var rows []MaterialReplica
	err := s.db.WithContext(ctx).Where("member_id = ? AND digest IN ?", memberID, digests).Find(&rows).Error
	return rows, err
}

// AdmittedMaterialManifests returns the pointers published by members whose
// admission to the cluster has not been revoked.
func (s *Store) AdmittedMaterialManifests(ctx context.Context, clusterID string) ([]MaterialManifestRecord, error) {
	var rows []MaterialManifestRecord
	err := s.db.WithContext(ctx).
		Joins("JOIN cluster_admissions ON cluster_admissions.key_fingerprint = material_manifest_records.source_key AND cluster_admissions.revoked_at IS NULL").
		Where("material_manifest_records.cluster_id = ?", clusterID).
		Find(&rows).Error
	return rows, err
}

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestMaterialConsumerReadsSelectOnlyRowsNamingFiles(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if rows, err := s.OfflineImageBlobRows(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("empty images: %#v %v", rows, err)
	}
	if rows, err := s.BenchmarkTokenizerRows(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("empty tokenizers: %#v %v", rows, err)
	}
	if rows, err := s.BenchmarkDetailRows(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("empty details: %#v %v", rows, err)
	}
	if rows, err := s.ModelLogoRows(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("empty logos: %#v %v", rows, err)
	}

	if err := s.db.Create(&[]Image{{Name: "with-blob", ImageRef: "example/with-blob:1", OfflineBlobPath: "images/a.tar", OfflineBlobSHA256: "aa", OfflineBlobBytes: 5}, {Name: "no-blob", ImageRef: "example/no-blob:1"}}).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := s.OfflineImageBlobRows(ctx); err != nil || len(rows) != 1 || rows[0].OfflineBlobPath != "images/a.tar" || rows[0].OfflineBlobSHA256 != "aa" || rows[0].OfflineBlobBytes != 5 {
		t.Fatalf("image blobs = %#v %v", rows, err)
	}
	if err := s.db.Create(&[]Model{{Name: "with-logo", Source: "local", SourceID: "with-logo", LogoPath: "logos/a.png"}, {Name: "no-logo", Source: "local", SourceID: "no-logo"}}).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ModelLogoRows(ctx); err != nil || len(rows) != 1 || rows[0].LogoPath != "logos/a.png" {
		t.Fatalf("model logos = %#v %v", rows, err)
	}
	if err := s.db.Create(&[]BenchmarkRun{{TokenizerRef: "tok/a.json"}, {}}).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := s.BenchmarkTokenizerRows(ctx); err != nil || len(rows) != 1 || rows[0].TokenizerRef != "tok/a.json" {
		t.Fatalf("tokenizers = %#v %v", rows, err)
	}
	if err := s.db.Create(&[]BenchmarkPoint{{RunID: 1, Seq: 1, DetailPath: "detail/a.json"}, {RunID: 1, Seq: 2}}).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := s.BenchmarkDetailRows(ctx); err != nil || len(rows) != 1 || rows[0].DetailPath != "detail/a.json" {
		t.Fatalf("details = %#v %v", rows, err)
	}

	now := time.Unix(1700000000, 0).UTC()
	replicas := []MaterialReplica{
		{Digest: "aa", MemberID: 7, Path: "materials/aa", RecordedAt: now},
		{Digest: "bb", MemberID: 7, Path: "materials/bb", RecordedAt: now},
		{Digest: "aa", MemberID: 8, Path: "materials/aa", RecordedAt: now},
	}
	if err := s.db.Create(&replicas).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountMaterialReplicas(ctx, "aa", 7); err != nil || n != 1 {
		t.Fatalf("count held = %d %v", n, err)
	}
	if n, err := s.CountMaterialReplicas(ctx, "bb", 8); err != nil || n != 0 {
		t.Fatalf("count missing = %d %v", n, err)
	}
	if row, err := s.MaterialReplicaByDigest(ctx, "bb", 7); err != nil || row.Path != "materials/bb" || row.MemberID != 7 {
		t.Fatalf("replica = %#v %v", row, err)
	}
	if _, err := s.MaterialReplicaByDigest(ctx, "bb", 8); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing replica err = %v", err)
	}
	rows, err := s.MaterialReplicasByDigests(ctx, 7, []string{"aa", "bb", "cc"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("replicas = %#v %v", rows, err)
	}
	for _, row := range rows {
		if row.MemberID != 7 {
			t.Fatalf("replica of another member returned: %#v", row)
		}
	}

	revoked := now
	admissions := []ClusterAdmission{
		{KeyFingerprint: "live-key", InvitationHash: "h1", Address: "10.0.0.1:9001", AdmittedAt: now},
		{KeyFingerprint: "gone-key", InvitationHash: "h2", Address: "10.0.0.2:9001", AdmittedAt: now, RevokedAt: &revoked},
	}
	if err := s.db.Create(&admissions).Error; err != nil {
		t.Fatal(err)
	}
	manifests := []MaterialManifestRecord{
		{SourceMemberID: 1, ClusterID: "c1", SourceKey: "live-key", Digest: "d1", Bytes: 1, RecordedAt: now},
		{SourceMemberID: 2, ClusterID: "c1", SourceKey: "gone-key", Digest: "d2", Bytes: 1, RecordedAt: now},
		{SourceMemberID: 3, ClusterID: "c2", SourceKey: "live-key", Digest: "d3", Bytes: 1, RecordedAt: now},
		{SourceMemberID: 4, ClusterID: "c1", SourceKey: "unknown-key", Digest: "d4", Bytes: 1, RecordedAt: now},
	}
	if err := s.db.Create(&manifests).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmittedMaterialManifests(ctx, "c1")
	if err != nil || len(admitted) != 1 || admitted[0].SourceMemberID != 1 || admitted[0].Digest != "d1" {
		t.Fatalf("admitted manifests = %#v %v", admitted, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.AdmittedMaterialManifests(canceled, "c1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read err = %v", err)
	}
	if _, err := s.CountMaterialReplicas(canceled, "aa", 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled count err = %v", err)
	}
}

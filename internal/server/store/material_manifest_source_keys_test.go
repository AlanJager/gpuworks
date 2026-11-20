package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The grant that brackets a material proof names only source keys whose
// admission is still live: a revoked or never-admitted key would be rejected
// and fail every round.
func TestAdmittedMaterialManifestSourceKeysLeavesOutRevokedAndUnknownSources(t *testing.T) {
	st, identity, admissions := membershipFixture(t)
	if keys, err := st.AdmittedMaterialManifestSourceKeys(t.Context(), identity.ClusterID); err != nil || len(keys) != 0 {
		t.Fatalf("no pointers published yet: keys=%v err=%v", keys, err)
	}
	never := strings.Repeat("9", 64)
	rows := []MaterialManifestRecord{
		{SourceMemberID: 1, ClusterID: identity.ClusterID, SourceKey: admissions[0].KeyFingerprint, Digest: strings.Repeat("a", 64), Bytes: 1, RecordedAt: time.Now().UTC()},
		{SourceMemberID: 2, ClusterID: identity.ClusterID, SourceKey: admissions[1].KeyFingerprint, Digest: strings.Repeat("b", 64), Bytes: 2, RecordedAt: time.Now().UTC()},
		{SourceMemberID: 3, ClusterID: identity.ClusterID, SourceKey: admissions[2].KeyFingerprint, Digest: strings.Repeat("c", 64), Bytes: 3, RecordedAt: time.Now().UTC()},
		{SourceMemberID: 4, ClusterID: identity.ClusterID, SourceKey: never, Digest: strings.Repeat("d", 64), Bytes: 4, RecordedAt: time.Now().UTC()},
		{SourceMemberID: 5, ClusterID: strings.Repeat("f", 32), SourceKey: admissions[0].KeyFingerprint, Digest: strings.Repeat("e", 64), Bytes: 5, RecordedAt: time.Now().UTC()},
	}
	if err := st.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeClusterAdmission(t.Context(), identity.ClusterID, admissions[2].KeyFingerprint); err != nil {
		t.Fatal(err)
	}
	keys, err := st.AdmittedMaterialManifestSourceKeys(t.Context(), identity.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{admissions[0].KeyFingerprint, admissions[1].KeyFingerprint}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys=%v want %v", keys, want)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if keys, err := st.AdmittedMaterialManifestSourceKeys(canceled, identity.ClusterID); !errors.Is(err, context.Canceled) || keys != nil {
		t.Fatalf("canceled read returned keys=%v err=%v", keys, err)
	}
}

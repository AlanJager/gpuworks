package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClusterAdmissionSnapshotPreservesReadContract(t *testing.T) {
	for _, name := range []string{"normal", "unknown_key", "invalid_key", "revoked", "missing_identity", "wrong_cluster", "wrong_protocol", "missing_identity_and_admission_table", "wrong_cluster_and_admission_table", "missing_admission_table", "canceled"} {
		t.Run(name, func(t *testing.T) {
			st, identity, _ := admissionFixture(t)
			key := strings.Repeat("c", 64)
			admitted, err := st.InitializeClusterSeedAdmission(t.Context(), identity.ClusterID, key, "127.0.0.1:9907")
			if err != nil {
				t.Fatal(err)
			}
			// Populate every non-revoked projection field, so a dropped SQL column
			// cannot hide behind the seed row's zero values.
			admitted.InvitationHash = strings.Repeat("f", 64)
			admitted.EnrollmentOrigin = "https://member.test:9443"
			admitted.MaterialGrantSequence = 7
			if err := st.DB().Save(&admitted).Error; err != nil {
				t.Fatal(err)
			}
			clusterID := identity.ClusterID
			ctx := t.Context()
			var want error
			switch name {
			case "unknown_key":
				key = strings.Repeat("d", 64)
				want = ErrClusterAdmissionRejected
			case "invalid_key":
				key = "invalid"
				want = ErrClusterAdmissionRejected
			case "revoked":
				if err := st.DB().Model(&ClusterAdmission{}).Where("key_fingerprint = ?", key).Update("revoked_at", time.Now().UTC()).Error; err != nil {
					t.Fatal(err)
				}
				want = ErrClusterAdmissionRejected
			case "missing_identity", "missing_identity_and_admission_table":
				if err := st.DB().Exec("DELETE FROM cluster_identities").Error; err != nil {
					t.Fatal(err)
				}
				want = ErrClusterNotInitialized
			case "wrong_cluster", "wrong_cluster_and_admission_table":
				clusterID = strings.Repeat("e", 32)
				want = ErrClusterIdentityConflict
			case "wrong_protocol":
				if err := st.DB().Model(&ClusterIdentity{}).Where("id = 1").Update("protocol", ClusterProtocolVersion+1).Error; err != nil {
					t.Fatal(err)
				}
				want = ErrClusterIdentityConflict
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if strings.HasSuffix(name, "_and_admission_table") || name == "missing_admission_table" {
				if err := st.DB().Exec("DROP TABLE cluster_admissions").Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, read := range []func(context.Context, string, string) (ClusterAdmission, error){st.ClusterAdmissionForVerifiedKey, st.ClusterAdmissionForCertificate} {
				got, err := read(ctx, clusterID, key)
				if name == "missing_admission_table" {
					if err == nil || errors.Is(err, ErrClusterNotInitialized) || errors.Is(err, ErrClusterIdentityConflict) || got != (ClusterAdmission{}) {
						t.Fatalf("database failure was hidden: %+v %v", got, err)
					}
				} else if want == nil {
					if err != nil || got != admitted {
						t.Fatalf("snapshot changed: %+v %v", got, err)
					}
				} else if !errors.Is(err, want) || got != (ClusterAdmission{}) {
					t.Fatalf("read=%+v err=%v want=%v", got, err, want)
				}
			}
			if !strings.HasSuffix(name, "_and_admission_table") && name != "missing_admission_table" {
				var current ClusterAdmission
				if err := st.DB().Where("key_fingerprint = ?", admitted.KeyFingerprint).Take(&current).Error; err != nil {
					t.Fatal(err)
				}
				if current.MaterialGrantSequence != admitted.MaterialGrantSequence {
					t.Fatal("observation consumed a material grant")
				}
			}
		})
	}
}

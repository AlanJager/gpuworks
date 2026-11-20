package store

import "testing"

// Writer ownership must not publish identity changes. Keep a real trigger to
// observe row-level side effects while each grant still mutates its admission.
func TestClusterGrantsDoNotRewriteIdentity(t *testing.T) {
	for _, path := range []string{"private", "transport", "business"} {
		t.Run(path, func(t *testing.T) {
			st, expected, admissions := membershipFixture(t)
			for _, query := range []string{
				"CREATE TABLE identity_write_observations (id INTEGER PRIMARY KEY)",
				"CREATE TRIGGER observe_identity_write AFTER UPDATE ON cluster_identities BEGIN INSERT INTO identity_write_observations (id) VALUES (NULL); END",
			} {
				if err := st.DB().Exec(query).Error; err != nil {
					t.Fatal(err)
				}
			}
			before, err := st.ClusterIdentity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			requester := admissions[1]
			for _, value := range []string{"first", "second"} {
				if path == "private" {
					if _, err := st.GrantClusterMaterials(t.Context(), expected.ClusterID, requester.KeyFingerprint); err != nil {
						t.Fatal(err)
					}
				} else if path == "transport" {
					revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), expected, requester.KeyFingerprint)
					if err != nil || revision == 0 || len(snapshot) != len(admissions) {
						t.Fatalf("transport lost authority checks: revision=%d admissions=%d err=%v", revision, len(snapshot), err)
					}
				} else {
					if err := st.SetBusinessSetting(t.Context(), SettingKey("writer-business-setting"), value); err != nil {
						t.Fatal(err)
					}
					if got := st.GetSetting(SettingKey("writer-business-setting")); got != value {
						t.Fatalf("business write did not commit: %q", got)
					}
				}
			}
			after, err := st.ClusterIdentity(t.Context())
			if err != nil || after != before {
				t.Fatalf("identity changed: before=%+v after=%+v err=%v", before, after, err)
			}
			if path == "business" {
				var notice CommittedNotification
				if err := st.DB().First(&notice, "request_key = ?", settingNotifyKey("writer-business-setting")).Error; err != nil || notice.Revision != 2 {
					t.Fatalf("business notification did not commit twice: revision=%d err=%v", notice.Revision, err)
				}
			}
			granted, err := st.ClusterAdmissionForVerifiedKey(t.Context(), expected.ClusterID, requester.KeyFingerprint)
			if err != nil || granted.MaterialGrantSequence != requester.MaterialGrantSequence+map[string]int64{"private": 2, "transport": 2, "business": 0}[path] {
				t.Fatalf("fresh committed increments lost: sequence=%d err=%v", granted.MaterialGrantSequence, err)
			}
			var writes int
			if err := st.DB().Raw("SELECT count(*) FROM identity_write_observations").Scan(&writes).Error; err != nil {
				t.Fatal(err)
			}
			if writes != 0 {
				t.Fatalf("writer acquisition rewrote immutable identity %d times", writes)
			}
		})
	}
}

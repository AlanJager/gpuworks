package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func isTransportGrantSnapshotQuery(tx *gorm.DB) bool {
	return tx.Error == nil && tx.Statement.SQL.String() == clusterTransportGrantSnapshotSQL
}

func TestTransportGrantCombinedRevisionSnapshotFullTupleRequests(t *testing.T) {
	st, identity, expected := membershipFixture(t)
	for i := range expected {
		expected[i].EnrollmentOrigin = fmt.Sprintf("https://peer-%d.example:9443", i)
		expected[i].AdmittedAt = time.Date(2026, 1, i+1, 2, 3, 4, 567000000, time.UTC)
		if err := st.db.Save(&expected[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	revoked, err := st.RevokeClusterAdmission(t.Context(), identity.ClusterID, expected[2].KeyFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	expected[2] = revoked
	for range 2 {
		expected[1].MaterialGrantSequence++
		calls := countMaterialGrantSQL(t, st, func() {
			revision, actual, err := st.GrantClusterTransportAdmissions(t.Context(), identity, expected[1].KeyFingerprint)
			if err != nil || revision != 4 || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("complete transport snapshot: revision=%d actual=%+v expected=%+v err=%v", revision, actual, expected, err)
			}
		})
		assertMaterialGrantSQL(t, calls, 8)
	}
}

func TestTransportGrantRevisionErrorPrecedesUnreadableSnapshot(t *testing.T) {
	for _, revisionFault := range []string{"missing", "zero", "negative", "text", "float", "null"} {
		for _, unreadable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unreadable-%t", revisionFault, unreadable), func(t *testing.T) {
				st, identity, admissions := membershipFixture(t)
				before := admissions[1]
				// Deliberately remove the schema constraint to exercise damaged
				// stored values, including NULL, rather than SQLite rejecting setup.
				if err := st.db.Exec("DROP TABLE cluster_authorizations").Error; err != nil {
					t.Fatal(err)
				}
				if err := st.db.Exec("CREATE TABLE cluster_authorizations(id INTEGER PRIMARY KEY, revision)").Error; err != nil {
					t.Fatal(err)
				}
				if revisionFault != "missing" {
					value := map[string]any{"zero": 0, "negative": -1, "text": "damaged", "float": 1.5, "null": nil}[revisionFault]
					if err := st.db.Exec("INSERT INTO cluster_authorizations(id,revision) VALUES(1,?)", value).Error; err != nil {
						t.Fatal(err)
					}
				}
				changed := false
				const hook = "test:transport-revision-before-unreadable-snapshot"
				if unreadable {
					if err := st.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*ClusterAdmission); !ok || tx.Error != nil || changed {
							return
						}
						changed = true
						tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("ALTER TABLE cluster_admissions RENAME TO unreadable_cluster_admissions").Error)
					}); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = st.db.Callback().Query().Remove(hook) }()
				}
				revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), identity, before.KeyFingerprint)
				if err == nil || err.Error() != "cluster authorization revision is missing or invalid" || revision != 0 || snapshot != nil || changed != unreadable {
					t.Fatalf("revision error lost precedence: changed=%t revision=%d snapshot=%+v err=%v", changed, revision, snapshot, err)
				}
				if unreadable {
					if err := st.db.Callback().Query().Remove(hook); err != nil {
						t.Fatal(err)
					}
				}
				var after ClusterAdmission
				if err := st.db.First(&after, "key_fingerprint=?", before.KeyFingerprint).Error; err != nil || after != before {
					t.Fatalf("failed revision retained mutation or renamed table: after=%+v err=%v", after, err)
				}
			})
		}
	}
}

func TestTransportGrantCombinedSnapshotPreservesValidation(t *testing.T) {
	for _, fault := range []string{"capacity-512", "capacity-513", "seed-missing", "invalid-key", "null-key", "noncanonical-address", "invalid-invitation"} {
		t.Run(fault, func(t *testing.T) {
			st, identity, admissions := membershipFixture(t)
			before := admissions[1]
			want := ErrClusterAdmissionRejected
			wantText := ""
			expectedCount := 3
			switch fault {
			case "capacity-512", "capacity-513":
				expectedCount = map[string]int{"capacity-512": 512, "capacity-513": 513}[fault]
				var extra []ClusterAdmission
				for i := 3; i < expectedCount; i++ {
					extra = append(extra, ClusterAdmission{KeyFingerprint: fmt.Sprintf("%064x", i), InvitationHash: fmt.Sprintf("%064x", i+1024), Address: fmt.Sprintf("extra-%d.example:9010", i), AdmittedAt: before.AdmittedAt})
				}
				if err := st.db.Create(&extra).Error; err != nil {
					t.Fatal(err)
				}
				wantText = "cluster admission snapshot exceeds transport policy capacity"
			case "seed-missing":
				if err := st.db.Exec("DELETE FROM cluster_admissions WHERE invitation_hash='' ").Error; err != nil {
					t.Fatal(err)
				}
				wantText = "cluster seed admission is missing"
			case "invalid-key":
				if err := st.db.Model(&ClusterAdmission{}).Where("key_fingerprint=?", admissions[2].KeyFingerprint).UpdateColumn("key_fingerprint", strings.ToUpper(admissions[2].KeyFingerprint)).Error; err != nil {
					t.Fatal(err)
				}
			case "null-key":
				// SQLite permits NULL in a non-integer primary key. A real
				// damaged row must not become the empty LEFT JOIN sentinel.
				if err := st.db.Exec("INSERT INTO cluster_admissions(key_fingerprint,invitation_hash,address) VALUES(NULL,?,?)", strings.Repeat("f", 64), "damaged.example:9010").Error; err != nil {
					t.Fatal(err)
				}
			case "noncanonical-address":
				if err := st.db.Model(&ClusterAdmission{}).Where("key_fingerprint=?", admissions[2].KeyFingerprint).UpdateColumn("address", "PEER-1.Example.:09010").Error; err != nil {
					t.Fatal(err)
				}
			case "invalid-invitation":
				if err := st.db.Model(&ClusterAdmission{}).Where("key_fingerprint=?", admissions[2].KeyFingerprint).UpdateColumn("invitation_hash", "damaged").Error; err != nil {
					t.Fatal(err)
				}
			}
			revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), identity, before.KeyFingerprint)
			if fault == "capacity-512" {
				if err != nil || revision != 3 || len(snapshot) != expectedCount {
					t.Fatalf("valid capacity boundary refused: revision=%d rows=%d err=%v", revision, len(snapshot), err)
				}
				return
			}
			if err == nil || revision != 0 || snapshot != nil || (wantText != "" && err.Error() != wantText) || (wantText == "" && !errors.Is(err, want)) {
				t.Fatalf("snapshot validation changed: revision=%d snapshot=%+v err=%v", revision, snapshot, err)
			}
			var after ClusterAdmission
			if err := st.db.First(&after, "key_fingerprint=?", before.KeyFingerprint).Error; err != nil || after != before {
				t.Fatalf("invalid snapshot committed grant: after=%+v err=%v", after, err)
			}
		})
	}
}

func TestTransportGrantCombinedReadFailureDoesNotRetryGrant(t *testing.T) {
	st, identity, admissions := membershipFixture(t)
	before := admissions[1]
	failure := errors.New("injected combined snapshot read failure")
	const hook = "test:transport-combined-read-error"
	failed := false
	if err := st.db.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if isTransportGrantSnapshotQuery(tx) {
			failed = true
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.db.Callback().Query().Remove(hook) }()
	calls := countMaterialGrantSQL(t, st, func() {
		revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), identity, before.KeyFingerprint)
		if !failed || !errors.Is(err, failure) || revision != 0 || snapshot != nil {
			t.Fatalf("failed combined read gained authority: injected=%t revision=%d snapshot=%+v err=%v", failed, revision, snapshot, err)
		}
	})
	increments, revisions, snapshots := 0, 0, 0
	for _, query := range calls {
		if strings.HasPrefix(query, "UPDATE `cluster_admissions`") {
			increments++
		}
		if strings.HasPrefix(query, "SELECT revision FROM cluster_authorizations") {
			revisions++
		}
		if strings.HasPrefix(query, "SELECT * FROM `cluster_admissions` ORDER BY key_fingerprint") {
			snapshots++
		}
		if query == "COMMIT" {
			t.Fatal("failed read committed")
		}
	}
	if increments != 1 || revisions != 1 || snapshots != 1 || calls[len(calls)-1] != "ROLLBACK" {
		t.Fatalf("error classification replayed grant or skipped original reads: %v", calls)
	}
	var after ClusterAdmission
	if err := st.db.First(&after, "key_fingerprint=?", before.KeyFingerprint).Error; err != nil || after != before {
		t.Fatalf("failed combined read left durable grant: after=%+v err=%v", after, err)
	}
}

func TestTransportGrantCombinedReadPreservesAdmissionQueryFailure(t *testing.T) {
	st, identity, admissions := membershipFixture(t)
	before := admissions[1]
	const hook = "test:transport-combined-unreadable-admissions"
	changed := false
	if err := st.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*ClusterAdmission); !ok || tx.Error != nil || changed {
			return
		}
		changed = true
		tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("ALTER TABLE cluster_admissions RENAME TO unreadable_cluster_admissions").Error)
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.db.Callback().Query().Remove(hook) }()
	revision, snapshot, err := st.GrantClusterTransportAdmissions(t.Context(), identity, before.KeyFingerprint)
	if !changed || err == nil || !strings.Contains(err.Error(), "no such table: cluster_admissions") || revision != 0 || snapshot != nil {
		t.Fatalf("valid revision suppressed original admission query failure: changed=%t revision=%d snapshot=%+v err=%v", changed, revision, snapshot, err)
	}
	if err := st.db.Callback().Query().Remove(hook); err != nil {
		t.Fatal(err)
	}
	var after ClusterAdmission
	if err := st.db.First(&after, "key_fingerprint=?", before.KeyFingerprint).Error; err != nil || after != before {
		t.Fatalf("unreadable snapshot retained grant or table rename: after=%+v err=%v", after, err)
	}
}

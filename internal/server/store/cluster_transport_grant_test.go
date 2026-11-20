package store

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestTransportGrantFinalObservationKeepsCompleteRecordBeforePhase(t *testing.T) {
	st, identity, admissions := membershipFixture(t)
	testTransportGrantFinalObservation(t, t.Context(), st, identity, admissions[1])
}

// Every fault is an actual same-transaction write after the original complete
// admissions snapshot. The final observation must reject it and roll it back.
func testTransportGrantFinalObservation(t *testing.T, ctx context.Context, st *Store, identity ClusterIdentity, before ClusterAdmission) {
	t.Helper()
	phase, err := st.ReadMaintenancePhase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"origin-and-phase-aba", "missing-and-freezing", "revoked", "sequence", "admitted-at", "phase-aba", "phase-missing", "phase-nonserving", "phase-query-error", "caller-cancel"} {
		t.Run(fault, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			const hook = "test:transport-grant-final-observation"
			changed := false
			if err := st.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if !isTransportGrantSnapshotQuery(tx) {
					return
				}
				changed = true
				write := tx.Session(&gorm.Session{NewDB: true})
				exec := func(query string, args ...any) { tx.AddError(write.Exec(query, args...).Error) }
				switch fault {
				case "origin-and-phase-aba":
					exec("UPDATE cluster_admissions SET enrollment_origin='https://late.example' WHERE key_fingerprint=?", before.KeyFingerprint)
					exec("UPDATE maintenance_phase SET phase='freezing',revision=revision+1")
					exec("UPDATE maintenance_phase SET phase='serving',revision=revision+1")
				case "missing-and-freezing":
					exec("DELETE FROM cluster_admissions WHERE key_fingerprint=?", before.KeyFingerprint)
					exec("UPDATE maintenance_phase SET phase='freezing',revision=revision+1")
				case "revoked":
					exec("UPDATE cluster_admissions SET revoked_at=CURRENT_TIMESTAMP WHERE key_fingerprint=?", before.KeyFingerprint)
				case "sequence":
					exec("UPDATE cluster_admissions SET material_grant_sequence=material_grant_sequence+1 WHERE key_fingerprint=?", before.KeyFingerprint)
				case "admitted-at":
					exec("UPDATE cluster_admissions SET admitted_at='2000-01-01 00:00:00' WHERE key_fingerprint=?", before.KeyFingerprint)
				case "phase-aba":
					exec("UPDATE maintenance_phase SET phase='freezing',revision=revision+1")
					exec("UPDATE maintenance_phase SET phase='serving',revision=revision+1")
				case "phase-missing":
					exec("DROP TRIGGER maintenance_phase_no_delete")
					exec("DELETE FROM maintenance_phase")
				case "phase-nonserving":
					exec("UPDATE maintenance_phase SET phase='freezing',revision=revision+1")
				case "phase-query-error":
					exec("ALTER TABLE maintenance_phase RENAME COLUMN revision TO unreadable_revision")
				case "caller-cancel":
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.db.Callback().Query().Remove(hook) }()
			want := ErrClusterAdmissionRejected
			switch fault {
			case "missing-and-freezing":
				want = gorm.ErrRecordNotFound
			case "phase-aba":
				want = ErrMaintenancePhaseChanged
			case "phase-missing":
				want = ErrMaintenancePhaseUnavailable
			case "phase-nonserving":
				want = ErrMaintenanceNotServing
			case "phase-query-error":
				want = nil
			case "caller-cancel":
				want = context.Canceled
			}
			revision, rows, err := st.GrantClusterTransportAdmissions(requestCtx, identity, before.KeyFingerprint)
			if !changed || err == nil || revision != 0 || rows != nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("post-snapshot fault lost check/order: changed%v revision%d rows%v err%v want%v", changed, revision, rows, err, want)
			}
			var after ClusterAdmission
			if err := st.db.WithContext(ctx).Where("key_fingerprint=?", before.KeyFingerprint).First(&after).Error; err != nil || after != before {
				t.Fatalf("post-snapshot admission mutation did not roll back: %+v err%v", after, err)
			}
			if afterPhase, err := st.ReadMaintenancePhase(ctx); err != nil || afterPhase != phase {
				t.Fatalf("post-snapshot phase mutation did not roll back: %+v err%v", afterPhase, err)
			}
		})
	}
}

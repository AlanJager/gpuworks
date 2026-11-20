package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"
)

// Maintenance phases are a replicated singleton. The phase is deliberately
// independent of the local .maintenance marker used by offline import.
// Transitions and writer authorization are installed separately; the schema
// starts existing clusters in their current serving state.
const (
	MaintenanceServing    = "serving"
	MaintenanceFreezing   = "freezing"
	MaintenanceFrozen     = "frozen"
	MaintenanceDrained    = "drained"
	MaintenanceUpgrading  = "upgrading"
	MaintenanceRecovering = "recovering"
)

// MaintenancePhaseState is the durable phase observed at one revision. A read
// alone is not a quorum barrier and must not authorize a later business write.
type MaintenancePhaseState struct {
	Phase    string
	Revision int64
}

var (
	ErrMaintenancePhaseUnavailable  = errors.New("maintenance phase is missing or invalid")
	ErrMaintenancePhaseChanged      = errors.New("maintenance phase revision changed")
	ErrMaintenanceTransitionInvalid = errors.New("invalid maintenance phase transition")
	ErrMaintenanceCommitUnconfirmed = errors.New("maintenance phase commit is unconfirmed")
	ErrMaintenanceNotServing        = errors.New("maintenance phase does not permit startup")
)

// ReadMaintenancePhase observes the replicated state without side effects.
// It can be stale during a quorum loss; only a confirmed conditional transition
// proves that a new phase has committed.
func (s *Store) ReadMaintenancePhase(ctx context.Context) (MaintenancePhaseState, error) {
	if err := ctx.Err(); err != nil {
		return MaintenancePhaseState{}, err
	}
	var state MaintenancePhaseState
	err := s.db.WithContext(ctx).Raw(`SELECT phase, revision FROM maintenance_phase WHERE id = 1`).Row().Scan(&state.Phase, &state.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenancePhaseState{}, ErrMaintenancePhaseUnavailable
	}
	if err != nil {
		return MaintenancePhaseState{}, err
	}
	if !validMaintenancePhase(state.Phase) || state.Revision <= 0 {
		return MaintenancePhaseState{}, ErrMaintenancePhaseUnavailable
	}
	return state, nil
}

// TransitionMaintenancePhase changes one legal edge only when the caller's
// complete observation still matches. The strict local scheduling pool refuses
// a follower; the real revision mutation requires a majority-confirmed commit.
// Any database/commit error returns no state and must not be blindly retried:
// the result may have committed before its acknowledgement was lost.
func (s *Store) TransitionMaintenancePhase(ctx context.Context, expected MaintenancePhaseState, next string) (MaintenancePhaseState, error) {
	if err := ctx.Err(); err != nil {
		return MaintenancePhaseState{}, err
	}
	if expected.Revision <= 0 || expected.Revision == math.MaxInt64 || !validMaintenanceEdge(expected.Phase, next) {
		return MaintenancePhaseState{}, ErrMaintenanceTransitionInvalid
	}
	var stale bool
	err := s.schedulingWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec(`UPDATE maintenance_phase
			SET phase = ?, revision = revision + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = 1 AND phase = ? AND revision = ?`, next, expected.Phase, expected.Revision)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			stale = true
			return ErrMaintenancePhaseChanged
		}
		return nil
	})
	if err != nil {
		if stale && errors.Is(err, ErrMaintenancePhaseChanged) {
			return MaintenancePhaseState{}, ErrMaintenancePhaseChanged
		}
		return MaintenancePhaseState{}, fmt.Errorf("%w: %w", ErrMaintenanceCommitUnconfirmed, err)
	}
	return MaintenancePhaseState{Phase: next, Revision: expected.Revision + 1}, nil
}

func validMaintenancePhase(phase string) bool {
	switch phase {
	case MaintenanceServing, MaintenanceFreezing, MaintenanceFrozen, MaintenanceDrained, MaintenanceUpgrading, MaintenanceRecovering:
		return true
	default:
		return false
	}
}

func validMaintenanceEdge(from, to string) bool {
	switch from {
	case MaintenanceServing:
		return to == MaintenanceFreezing
	case MaintenanceFreezing:
		return to == MaintenanceFrozen || to == MaintenanceServing
	case MaintenanceFrozen:
		return to == MaintenanceDrained || to == MaintenanceServing
	case MaintenanceDrained:
		return to == MaintenanceUpgrading || to == MaintenanceServing
	case MaintenanceUpgrading:
		return to == MaintenanceRecovering
	case MaintenanceRecovering:
		return to == MaintenanceServing
	default:
		return false
	}
}

const (
	maintenancePhaseRevisionTrigger = `CREATE TRIGGER maintenance_phase_revision
		BEFORE UPDATE ON maintenance_phase
		WHEN NEW.revision != OLD.revision + 1 OR NEW.id != OLD.id
		BEGIN SELECT RAISE(ABORT, 'maintenance phase revision must advance by one'); END`
	maintenancePhaseNoDeleteTrigger = `CREATE TRIGGER maintenance_phase_no_delete
		BEFORE DELETE ON maintenance_phase
		BEGIN SELECT RAISE(ABORT, 'maintenance phase must not be deleted'); END`
	// REPLACE can bypass a DELETE trigger in SQLite. Reject any second insert
	// before its conflict action can discard the existing revision.
	maintenancePhaseNoReplaceTrigger = `CREATE TRIGGER maintenance_phase_no_replace
		BEFORE INSERT ON maintenance_phase
		WHEN EXISTS (SELECT 1 FROM maintenance_phase WHERE id = 1)
		BEGIN SELECT RAISE(ABORT, 'maintenance phase already exists'); END`
)

func maintenancePhaseTrigger(name string) (string, bool) {
	switch name {
	case "maintenance_phase_revision":
		return maintenancePhaseRevisionTrigger, true
	case "maintenance_phase_no_delete":
		return maintenancePhaseNoDeleteTrigger, true
	case "maintenance_phase_no_replace":
		return maintenancePhaseNoReplaceTrigger, true
	default:
		return "", false
	}
}

func migrateMaintenancePhase(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE maintenance_phase (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		phase TEXT NOT NULL CHECK (phase IN ('serving', 'freezing', 'frozen', 'drained', 'upgrading', 'recovering')),
		revision INTEGER NOT NULL CHECK (revision > 0),
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		return err
	}
	for _, name := range []string{"maintenance_phase_revision", "maintenance_phase_no_delete", "maintenance_phase_no_replace"} {
		sql, _ := maintenancePhaseTrigger(name)
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return db.Exec(`INSERT INTO maintenance_phase (id, phase, revision, updated_at)
		VALUES (1, ?, 1, CURRENT_TIMESTAMP)`, MaintenanceServing).Error
}

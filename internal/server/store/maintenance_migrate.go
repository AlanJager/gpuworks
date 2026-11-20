package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migrateSchemaOffline is only called through an existing database's runtime
// lease. It does not seed defaults or recover business sessions. A pre-phase
// schema can be upgraded explicitly; a known non-serving phase stays closed.
func (s *Store) migrateSchemaOffline(ctx context.Context) error {
	// Store holds mutexes, so the offline pass uses its own value instead of copying s.
	initializing := &Store{db: s.schedulingWriter().WithContext(ctx)}
	initializing.readDB = initializing.db
	if err := checkKnownSchema(initializing.db); err != nil {
		return err
	}
	var markerTable int
	if err := initializing.db.Raw(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'`).Row().Scan(&markerTable); err != nil {
		return err
	}
	if markerTable != 1 {
		return fmt.Errorf("%w: offline migration requires an established schema", ErrSchemaIncompatible)
	}
	// This tree numbers this migration 243. Match the row by name so an ordinary
	// pre-phase database is upgraded instead of rejected for a different 115.
	var markerName string
	err := initializing.db.Raw(`SELECT name FROM schema_migrations WHERE name = ?`, "maintenance_phase").Row().Scan(&markerName)
	switch {
	case err == nil:
		if markerName != "maintenance_phase" {
			return ErrMaintenancePhaseUnavailable
		}
		if err := initializing.RequireServingMaintenancePhase(ctx); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		var phaseTable int
		if err := initializing.db.Raw(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'maintenance_phase'`).Row().Scan(&phaseTable); err != nil {
			return err
		}
		if phaseTable != 0 {
			return ErrMaintenancePhaseUnavailable
		}
	case err != nil:
		return err
	}
	if err := initializing.migrate(); err != nil {
		return err
	}
	return initializing.RequireServingMaintenancePhase(ctx)
}

// RequireServingMaintenancePhase checks startup intent. This observation does
// not authorize a later business write; writer fencing remains separate.
func (s *Store) RequireServingMaintenancePhase(ctx context.Context) error {
	state, err := s.ReadMaintenancePhase(ctx)
	if err != nil {
		return err
	}
	if state.Phase != MaintenanceServing {
		return fmt.Errorf("%w: %s at revision %d", ErrMaintenanceNotServing, state.Phase, state.Revision)
	}
	return nil
}

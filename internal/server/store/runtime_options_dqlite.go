//go:build linux && dqlite

package store

import "context"

// initializeExtensionSchemas applies the extension steps where
// InitializeSchema applied the core ones: on the local leader connection of
// a replicated store, bounded by ctx.
func (s *Store) initializeExtensionSchemas(ctx context.Context, sets []MigrationSet) error {
	db := s.db
	if s.schedulingDB != nil {
		db = s.schedulingDB
	}
	if err := migrateNamespaces(db.WithContext(ctx), sets); err != nil {
		return err
	}
	s.extensions = append([]MigrationSet(nil), sets...)
	return nil
}

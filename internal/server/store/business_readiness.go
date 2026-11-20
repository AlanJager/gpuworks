package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// CheckBusinessReads exercises public settings and the node and login tables.
// It returns no business data and does not seed, recover, or authorize writes.
// Ordinary replicated reads may reach the leader; the HTTP entry must retain
// its separate current membership and material admission checks.
func (s *Store) CheckBusinessReads(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Store holds mutexes, so this read uses its own value instead of copying s.
	scoped := &Store{db: s.db.WithContext(ctx)}
	if err := scoped.RequireServingMaintenancePhase(ctx); err != nil {
		return err
	}
	settings, err := scoped.AllSettings()
	if err != nil {
		return err
	}
	for _, value := range settings {
		if value != "" && !json.Valid([]byte(value)) {
			return errors.New("public setting document is invalid")
		}
	}
	// Bounded real column reads detect broken business tables without loading
	// a fleet or publishing node details, login identities, or password hashes.
	var nodeIDs []uint
	if err := scoped.db.Model(&Node{}).Limit(1).Pluck("id", &nodeIDs).Error; err != nil {
		return err
	}
	var users []User
	if err := scoped.db.Limit(1).Find(&users).Error; err != nil {
		return err
	}
	return ctx.Err()
}

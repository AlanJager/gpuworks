package store

import (
	"context"
	"fmt"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Pushed state is the tables plugins push (ADR 0071 decision 8), kept in the
// typed core tables of package pstate. Package pushstate is their only
// writer: it batches every change of one writer tick into one
// WritePushedState, and reads them all once at boot with LoadPushedState.

// createPushedStateTables creates the pushed state tables.
func createPushedStateTables(db *gorm.DB) error {
	return db.AutoMigrate(pstate.Models()...)
}

// createCapacityKeepTable creates the table of the nodes a fleet capacity
// table keeps first, on a database whose pushed state tables predate it.
func createCapacityKeepTable(db *gorm.DB) error {
	return db.AutoMigrate(&pstate.CapacityKeep{})
}

// pushedBatchSize bounds the rows of one INSERT.
const pushedBatchSize = 500

// LoadPushedState reads every row of pushed state in one read transaction.
// The tables are bounded by the kinds' row limits, so this is a boot-time
// read, never a per-event one.
func (s *Store) LoadPushedState(ctx context.Context) (pstate.Snapshot, error) {
	var snap pstate.Snapshot
	err := s.reader().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r := &snap.Rows
		for _, q := range []struct {
			dest  any
			order string
		}{
			{&snap.Owners, "owner, kind"},
			{&r.Availability, "owner"},
			{&r.AvailabilityRows, "owner, feature"},
			{&r.AvailabilitySteps, "owner, feature, step"},
			{&r.AvailabilityLimits, "owner, feature, step, dim"},
			{&r.FleetCapacity, "owner"},
			{&r.CapacitySteps, "owner, step"},
			{&r.CapacityKeep, "owner, node_id"},
			{&r.NodeHolds, "owner, node_id"},
			{&r.Banners, "owner, banner_id"},
			{&r.BannerSteps, "owner, banner_id, step"},
			{&r.Attestations, "owner, name"},
			{&r.Params, "owner, kind, entry, slot, key"},
			{&snap.Down, "owner"},
			{&snap.Overrides, "id"},
			{&snap.Markers, "name"},
		} {
			if err := tx.Order(q.order).Find(q.dest).Error; err != nil {
				return err
			}
		}
		var clocks []pstate.Clock
		if err := tx.Where("id = ?", pstate.ClockID).Find(&clocks).Error; err != nil {
			return err
		}
		if len(clocks) == 1 {
			snap.Clock = &clocks[0]
		}
		return nil
	})
	return snap, err
}

// WritePushedState commits b in one writer transaction. A table write whose
// generation does not exceed the stored one is skipped and reported in
// Result.Stale; any other failure rolls the whole batch back.
func (s *Store) WritePushedState(ctx context.Context, b *pstate.Batch) (pstate.Result, error) {
	var res pstate.Result
	if b.Empty() {
		return res, nil
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res = pstate.Result{}
		for _, owner := range b.Uninstall {
			if err := deleteOwnerRows(tx, owner); err != nil {
				return err
			}
		}
		for _, k := range b.Clear {
			if err := clearPushedTable(tx, k.Owner, k.Kind); err != nil {
				return err
			}
		}
		for i := range b.Tables {
			stale, err := writePushedTable(tx, &b.Tables[i])
			if err != nil {
				return err
			}
			if stale {
				res.Stale = append(res.Stale, pstate.OwnerKind{Owner: b.Tables[i].Record.Owner, Kind: b.Tables[i].Record.Kind})
			}
		}
		for _, d := range b.Down {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&d).Error; err != nil {
				return err
			}
		}
		if len(b.Up) > 0 {
			if err := tx.Where("owner IN ?", b.Up).Delete(&pstate.Down{}).Error; err != nil {
				return err
			}
		}
		if len(b.DropOverrides) > 0 {
			if err := tx.Where("id IN ?", b.DropOverrides).Delete(&pstate.Override{}).Error; err != nil {
				return err
			}
		}
		for _, o := range b.Overrides {
			o.ID = 0
			if err := tx.Create(&o).Error; err != nil {
				return err
			}
			res.OverrideIDs = append(res.OverrideIDs, o.ID)
		}
		if b.Clock != nil {
			c := *b.Clock
			c.ID = pstate.ClockID
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&c).Error; err != nil {
				return err
			}
		}
		for _, m := range b.Markers {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return pstate.Result{}, err
	}
	return res, nil
}

// writePushedTable applies one table write in tx; stale reports a write
// skipped because the stored generation is not below its own.
func writePushedTable(tx *gorm.DB, w *pstate.TableWrite) (stale bool, err error) {
	rec := w.Record
	var stored []pstate.Owner
	if err := tx.Where("owner = ? AND kind = ?", rec.Owner, rec.Kind).Find(&stored).Error; err != nil {
		return false, err
	}
	if len(stored) == 1 && stored[0].Generation >= rec.Generation {
		return true, nil
	}
	if err := deleteKindRows(tx, rec.Owner, rec.Kind, w.DeleteAll, w.DeleteEntries); err != nil {
		return false, err
	}
	if err := insertPushedRows(tx, &w.Insert); err != nil {
		return false, err
	}
	return false, tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&rec).Error
}

// kindTables are the tables of each kind's rows and the column naming an
// entry in each, the notice parameters aside.
var kindTables = map[string][]struct {
	model  any
	column string
}{
	extapi.KindAvailability: {
		{&pstate.AvailabilityRow{}, "feature"}, {&pstate.AvailabilityStep{}, "feature"},
		{&pstate.AvailabilityLimit{}, "feature"}, {&pstate.AvailabilityTable{}, ""},
	},
	extapi.KindFleetCapacity: {{&pstate.FleetCapacity{}, ""}, {&pstate.CapacityStep{}, ""}, {&pstate.CapacityKeep{}, ""}},
	extapi.KindNodeHolds:     {{&pstate.NodeHold{}, "node_id"}},
	extapi.KindBanners:       {{&pstate.Banner{}, "banner_id"}, {&pstate.BannerStep{}, "banner_id"}},
	extapi.KindAttestations:  {{&pstate.Attestation{}, "name"}},
}

// deleteKindRows deletes an owner's rows of kind: all of them, or those of
// entries. A table without an entry column (a kind's header row) is
// deleted only with all.
func deleteKindRows(tx *gorm.DB, owner, kind string, all bool, entries []string) error {
	tables, ok := kindTables[kind]
	if !ok {
		return fmt.Errorf("pushed state: unknown kind %q", kind)
	}
	if !all && len(entries) == 0 {
		return nil
	}
	for _, t := range tables {
		q := tx.Where("owner = ?", owner)
		switch {
		case all:
		case t.column == "":
			continue
		case t.column == "node_id":
			ids, err := nodeIDEntries(entries)
			if err != nil {
				return err
			}
			q = q.Where("node_id IN ?", ids)
		default:
			q = q.Where(t.column+" IN ?", entries)
		}
		if err := q.Delete(t.model).Error; err != nil {
			return err
		}
	}
	q := tx.Where("owner = ? AND kind = ?", owner, kind)
	if !all {
		q = q.Where("entry IN ?", entries)
	}
	return q.Delete(&pstate.NoticeParam{}).Error
}

func nodeIDEntries(entries []string) ([]int64, error) {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		id, err := strconv.ParseInt(e, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pushed state: node entry %q is not a node id", e)
		}
		ids[i] = id
	}
	return ids, nil
}

// insertPushedRows inserts every row of r, replacing a row with its key:
// a kind's header row (pstate_availability) has no entry and is replaced
// in place rather than deleted first.
func insertPushedRows(tx *gorm.DB, r *pstate.Rows) error {
	for _, rows := range []struct {
		n    int
		rows any
	}{
		{len(r.Availability), &r.Availability},
		{len(r.AvailabilityRows), &r.AvailabilityRows},
		{len(r.AvailabilitySteps), &r.AvailabilitySteps},
		{len(r.AvailabilityLimits), &r.AvailabilityLimits},
		{len(r.FleetCapacity), &r.FleetCapacity},
		{len(r.CapacitySteps), &r.CapacitySteps},
		{len(r.CapacityKeep), &r.CapacityKeep},
		{len(r.NodeHolds), &r.NodeHolds},
		{len(r.Banners), &r.Banners},
		{len(r.BannerSteps), &r.BannerSteps},
		{len(r.Attestations), &r.Attestations},
		{len(r.Params), &r.Params},
	} {
		if rows.n == 0 {
			continue
		}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(rows.rows, pushedBatchSize).Error; err != nil {
			return err
		}
	}
	return nil
}

// clearPushedTable deletes owner's table of kind in tx: its rows, its
// owner record and the administrator's overrides of it, so the table is
// absent as if never pushed and the owner's next push of any generation is
// accepted. Clearing a table that is not there deletes nothing. Package
// pushstate calls it through WritePushedState (pstate.Batch.Clear), in the
// transaction of one writer tick.
func clearPushedTable(tx *gorm.DB, owner, kind string) error {
	if err := deleteKindRows(tx, owner, kind, true, nil); err != nil {
		return err
	}
	if err := tx.Where("owner = ? AND kind = ?", owner, kind).Delete(&pstate.Owner{}).Error; err != nil {
		return err
	}
	return tx.Where("owner = ? AND kind = ?", owner, kind).Delete(&pstate.Override{}).Error
}

// deleteOwnerRows deletes every row an owner has in pushed state.
func deleteOwnerRows(tx *gorm.DB, owner string) error {
	for _, model := range pstate.Models() {
		switch model.(type) {
		case *pstate.Clock, *pstate.Marker:
			continue
		}
		if err := tx.Where("owner = ?", owner).Delete(model).Error; err != nil {
			return err
		}
	}
	return nil
}

package store

import "gorm.io/gorm"

// PruneAgentUpgrades bounds finished history. An unresolved reservation or
// remaining queue is operational state and is never removed by retention.
func (s *Store) PruneAgentUpgrades(keep int) error {
	if keep < 1 {
		keep = 1
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var runs []AgentUpgradeRun
		if err := tx.Order("id desc").Find(&runs).Error; err != nil {
			return err
		}
		var items []AgentUpgradeItem
		if err := tx.Find(&items).Error; err != nil {
			return err
		}
		protected := map[uint]bool{}
		for _, i := range items {
			if i.Phase == "queued" || i.Phase == "transferring" || i.Phase == "waiting_registration" || i.Phase == "observing_recovery" || (i.Phase == "unconfirmed" && i.VerifiedAt == nil) {
				protected[i.RunID] = true
			}
		}
		finished := 0
		var remove []uint
		for _, r := range runs {
			if protected[r.ID] {
				continue
			}
			finished++
			if finished > keep {
				remove = append(remove, r.ID)
			}
		}
		if len(remove) == 0 {
			return nil
		}
		if err := tx.Where("run_id IN ?", remove).Delete(&AgentUpgradeItem{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", remove).Delete(&AgentUpgradeRun{}).Error
	})
}

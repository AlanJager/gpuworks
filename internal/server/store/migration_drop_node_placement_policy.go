package store

import "gorm.io/gorm"

// dropNodePlacementPolicy removes the node row's placement policy column.
// Whether work may go on a card that processes outside the platform hold is
// an operator's decision about that card (card_scheduling.go), not a setting
// of its node (ADR 0068).
//
// A node that had the setting on is not carried over to its cards: enabling
// a card lets every kind of work onto it, which is more than the setting
// allowed. Its instances keep running; new placements leave held cards alone
// until an operator enables them.
func dropNodePlacementPolicy(db *gorm.DB) error {
	present, err := tableHasColumn(db, "nodes", "placement_share_with_foreign")
	if err != nil || !present {
		return err
	}
	return dropColumns(db, "nodes", "placement_share_with_foreign")
}

package store

import (
	"context"

	"gorm.io/gorm"
)

// schedulingWriteTransaction acquires the local pool's writer before reading
// a generation or decision snapshot. Like clusterWriteTransaction, the
// zero-row update prevents a competing grant from making a deferred read's
// write upgrade fail. It is only lock acquisition; the real mutation and
// confirmed commit remain necessary for authority. Nothing is retried.
// A cluster member's write pool takes the writer itself before its admission
// read; this statement stays for every store that pool does not check: a
// single server, and a replicated store not bound to a member.
func (s *Store) schedulingWriteTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	return s.schedulingWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE cluster_identities SET id = id WHERE 0").Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

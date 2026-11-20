package store

import (
	"context"
	"database/sql"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// GORM begins outside database/sql's connection acquisition. Transaction views
// retain the returned *sql.Tx, so nested transactions use savepoints without
// acquiring this admission again. Plain queries use the original SQL pool.
type replicatedWriterPool struct{ *sql.DB }

func (p *replicatedWriterPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := cluster.BeginWriterTransaction(ctx, p.DB, options)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func (p *replicatedWriterPool) GetDBConn() (*sql.DB, error) { return p.DB, nil }

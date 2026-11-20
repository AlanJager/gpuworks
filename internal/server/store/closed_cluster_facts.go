package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func bindClosedFactsTx(ctx context.Context, tx *sql.Tx) (*gorm.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("closed cluster facts require a borrowed SQL transaction")
	}
	db, err := gorm.Open(replicatedDialect{sqlite.Dialector{Conn: tx}}, &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	return db.WithContext(ctx), nil
}

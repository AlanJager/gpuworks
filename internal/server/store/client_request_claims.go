package store

import (
	"errors"
)

var ErrClientRequestOperationMismatch = errors.New("client request key belongs to another operation")

type ClientRequestClaim struct {
	OwnerID    uint   `gorm:"primaryKey"`
	RequestKey string `gorm:"primaryKey;size:128"`
	Operation  string `gorm:"size:32;not null"`
}

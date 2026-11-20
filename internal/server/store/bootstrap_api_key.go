package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func prepareAPIKey(name string, userID uint) (string, *APIKey, error) {
	full, prefix := auth.NewAPIKey()
	hash, err := auth.HashPassword(full)
	if err != nil {
		return "", nil, err
	}
	if prefix == "" {
		return "", nil, errors.New("api key prefix is empty")
	}
	return full, &APIKey{Name: name, Prefix: prefix, Hash: hash, UserID: userID}, nil
}

func insertAPIKey(tx *gorm.DB, k *APIKey) error {
	if err := tx.Create(k).Error; err != nil {
		return err
	}
	return saveUnpublishedNotification(tx, CommittedNotification{RequestKey: apiKeyNotifyKey(k.Prefix), ResourceID: k.Prefix, Topic: "apikey", Reason: "created"})
}

// EnsureDefaultAPIKey creates a default only when the serialized transaction
// still observes an empty key set. Existing configuration is a read-only no-op.
// Unknown commit outcomes return no creation fact and must not be blindly replayed.
func (s *Store) EnsureDefaultAPIKey() (bool, error) {
	has, err := s.HasAnyAPIKeyContext(context.Background())
	if err != nil || has {
		return false, err
	}
	_, k, err := prepareAPIKey("default", 0)
	if err != nil {
		return false, err
	}
	created := false
	err = s.withBusinessWriteTx(context.Background(), func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&APIKey{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		if err := insertAPIKey(tx, k); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if created && s.Bus != nil {
		_ = s.publishCommittedNotification(apiKeyNotifyKey(k.Prefix))
	}
	return created, nil
}

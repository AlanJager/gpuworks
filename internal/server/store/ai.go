package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

const encryptedAIKeyPrefix = "enc:v1:"

func EncryptAIKey(box *auth.Secretbox, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	if box == nil {
		return "", errors.New("AI key encryption is not configured")
	}
	encoded, err := box.Encrypt([]byte(key))
	if err != nil {
		return "", err
	}
	return encryptedAIKeyPrefix + encoded, nil
}

func DecryptAIKey(box *auth.Secretbox, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, encryptedAIKeyPrefix) {
		// Early releases stored the key in plaintext. Keep compatibility for
		// old rows; new writes are always encrypted by the API before
		// persistence.
		return value, nil
	}
	if box == nil {
		return "", errors.New("AI key decryption is not configured")
	}
	plain, err := box.Decrypt(strings.TrimPrefix(value, encryptedAIKeyPrefix))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// CreateAIConfig stores an LLM endpoint config. If it's the first or marked
// default, it becomes the default (unsetting others).
func (s *Store) CreateAIConfig(c *AIConfig) (*AIConfig, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&AIConfig{}).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			c.IsDefault = true
		}
		if c.IsDefault {
			if err := tx.Model(&AIConfig{}).Where("is_default = ?", true).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Create(c).Error
	})
	if err != nil {
		return nil, err
	}
	s.Bus.PublishChange("ai", uintKey(c.ID), "created")
	return c, nil
}

// ListAIConfigs returns all AI configs.
func (s *Store) ListAIConfigs() ([]AIConfig, error) {
	var cs []AIConfig
	err := s.db.Order("id desc").Find(&cs).Error
	return cs, err
}

// DefaultAIConfig returns the default AI config, or ErrNotFound if none.
func (s *Store) DefaultAIConfig() (*AIConfig, error) {
	var c AIConfig
	err := s.db.Where("is_default = ?", true).First(&c).Error
	if err != nil {
		// Fall back to any config.
		if err2 := s.db.First(&c).Error; err2 == nil {
			return &c, nil
		}
		return nil, ErrNotFound
	}
	return &c, nil
}

// DeleteAIConfig removes a config.
func (s *Store) DeleteAIConfig(id uint) error {
	return s.db.Delete(&AIConfig{}, id).Error
}

// HasAIConfig reports whether any LLM endpoint is configured.
func (s *Store) HasAIConfig() bool {
	var count int64
	s.db.Model(&AIConfig{}).Count(&count)
	return count > 0
}

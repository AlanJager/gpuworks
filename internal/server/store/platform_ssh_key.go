package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// platformSSHKeyID is the only row: a server has exactly one platform key.
const platformSSHKeyID = 1

// PlatformSSHKey is the server's own SSH key pair, used to log in to nodes
// whose authorized_keys lists PublicKey. The private key is stored as a
// secretbox ciphertext, like node SSH credentials.
type PlatformSSHKey struct {
	ID               uint   `gorm:"primaryKey"`
	PrivateKeyCipher string `gorm:"size:8192"`
	PublicKey        string `gorm:"size:2048"` // one authorized_keys line
	Fingerprint      string `gorm:"size:128"`  // SHA256:...
	CreatedAt        time.Time
}

func (PlatformSSHKey) TableName() string { return "platform_ssh_keys" }

// PlatformSSHKey returns the platform key and its decrypted private key, or
// ErrNotFound before one has been generated or imported.
func (s *Store) PlatformSSHKey() (PlatformSSHKey, string, error) {
	if s.secretbox == nil {
		return PlatformSSHKey{}, "", errors.New("ssh credential encryption is not configured")
	}
	var row PlatformSSHKey
	if err := s.reader().Where("id = ?", platformSSHKeyID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PlatformSSHKey{}, "", ErrNotFound
		}
		return PlatformSSHKey{}, "", err
	}
	raw, err := s.secretbox.Decrypt(row.PrivateKeyCipher)
	if err != nil {
		return PlatformSSHKey{}, "", fmt.Errorf("decrypt platform ssh key: %w", err)
	}
	return row, string(raw), nil
}

// SetPlatformSSHKey replaces the platform key. The caller has validated the
// key and derived publicKey and fingerprint from it.
func (s *Store) SetPlatformSSHKey(privatePEM, publicKey, fingerprint string) (PlatformSSHKey, error) {
	if s.secretbox == nil {
		return PlatformSSHKey{}, errors.New("ssh credential encryption is not configured")
	}
	if strings.TrimSpace(privatePEM) == "" || strings.TrimSpace(publicKey) == "" || strings.TrimSpace(fingerprint) == "" {
		return PlatformSSHKey{}, errors.New("platform ssh key, public key and fingerprint are required")
	}
	ciphertext, err := s.secretbox.Encrypt([]byte(privatePEM))
	if err != nil {
		return PlatformSSHKey{}, fmt.Errorf("encrypt platform ssh key: %w", err)
	}
	row := PlatformSSHKey{
		ID: platformSSHKeyID, PrivateKeyCipher: ciphertext,
		PublicKey: strings.TrimSpace(publicKey), Fingerprint: strings.TrimSpace(fingerprint),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.Save(&row).Error; err != nil {
		return PlatformSSHKey{}, err
	}
	return row, nil
}

package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// Secretbox encrypts small secrets (cloud credentials) with AES-256-GCM using a
// key derived from a server master secret. Ciphertext is base64(nonce||ct).
type Secretbox struct {
	gcm cipher.AEAD
}

// NewSecretbox derives an AES-256 key from the master secret (sha256) and builds
// a GCM box.
func NewSecretbox(masterSecret string) (*Secretbox, error) {
	if masterSecret == "" {
		return nil, errors.New("empty master secret")
	}
	key := sha256.Sum256([]byte(masterSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Secretbox{gcm: gcm}, nil
}

// Encrypt returns base64(nonce||ciphertext).
func (s *Secretbox) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := s.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt.
func (s *Secretbox) Decrypt(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	ns := s.gcm.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	return s.gcm.Open(nil, nonce, ct, nil)
}

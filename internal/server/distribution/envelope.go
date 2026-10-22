package distribution

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUntrusted is an index whose envelope carries no signature from a trusted
// key.
var ErrUntrusted = errors.New("index is not signed by a trusted key")

// Envelope is the published form of an index: the payload bytes exactly as
// signed, and the signatures over them. Keeping both in one file means a
// reader never pairs a new index with an old signature while a publish is
// halfway through.
type Envelope struct {
	Payload    []byte      `json:"payload"`
	Signatures []Signature `json:"signatures"`
}

// Signature is one ed25519 signature over an envelope's payload.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   []byte `json:"sig"`
}

// Keys maps key ids to the public keys an index may be signed with. During a
// rotation a build trusts the old and the new key.
type Keys map[string]ed25519.PublicKey

// Seal signs payload and returns the envelope as published.
func Seal(payload []byte, keyID string, key ed25519.PrivateKey) ([]byte, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, errors.New("seal index: key id is required")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("seal index: not an ed25519 private key")
	}
	return json.MarshalIndent(Envelope{
		Payload:    payload,
		Signatures: []Signature{{KeyID: keyID, Sig: ed25519.Sign(key, payload)}},
	}, "", "  ")
}

// Open verifies an envelope and returns its payload and the id of the key that
// verified it. Signatures by keys the build does not know are skipped, so a
// rotation can add a signature before every client trusts its key.
func Open(raw []byte, trusted Keys) ([]byte, string, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", fmt.Errorf("decode index envelope: %w", err)
	}
	if len(envelope.Payload) == 0 {
		return nil, "", errors.New("index envelope has no payload")
	}
	var bad error
	for _, sig := range envelope.Signatures {
		key, ok := trusted[sig.KeyID]
		if !ok || len(key) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(key, envelope.Payload, sig.Sig) {
			return envelope.Payload, sig.KeyID, nil
		}
		if bad == nil {
			bad = fmt.Errorf("index signature by %s does not verify", sig.KeyID)
		}
	}
	if bad != nil {
		return nil, "", bad
	}
	return nil, "", ErrUntrusted
}

// PrivateKeyFromSeed decodes a base64 ed25519 seed (RFC 8032 private key), the
// form the signing key is kept in.
func PrivateKeyFromSeed(encoded string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key is %d bytes, want a %d-byte ed25519 seed", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKey decodes a base64 ed25519 public key.
func PublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

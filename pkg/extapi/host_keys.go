package extapi

import "regexp"

// Object names of the host API's key derivation.
const (
	objectKeyRequest = "key_request"
	objectDerivedKey = "derived_key"
)

// PathHostDeriveKey answers POST of a KeyRequest with a DerivedKey. It
// needs the permission crypto:derive_key and CapHostDeriveKey. The key is
// derived from the server's master key and bound to the calling package and
// the label, so it is the same on every call for the life of the master
// key, another package never gets it, and it reveals nothing of the master
// key.
const PathHostDeriveKey = "/host/v1/derive-key"

// Bounds of a derived key, in bytes.
const (
	MinDerivedKey = 16
	MaxDerivedKey = 64
)

// keyLabelPattern is a key's label.
var keyLabelPattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// ValidKeyLabel reports whether label is a key label: 1 to 64 of
// [a-z0-9._-].
func ValidKeyLabel(label string) bool { return keyLabelPattern.MatchString(label) }

// KeyRequest is the body of POST /host/v1/derive-key.
type KeyRequest struct {
	// Label names the key within the package, so one package derives as
	// many unrelated keys as it has labels.
	Label string `json:"label"`
	// Length is the key's length in bytes, MinDerivedKey to MaxDerivedKey.
	Length int `json:"length"`
}

// DecodeKeyRequest decodes and validates a key request.
func DecodeKeyRequest(data []byte) (*KeyRequest, error) {
	if len(data) > MaxHostCallBytes {
		return nil, malformed(objectKeyRequest, &fieldError{rule: RuleRange})
	}
	var r KeyRequest
	if err := decodeStrict(objectKeyRequest, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the request's rules.
func (r *KeyRequest) Validate() error {
	if f := first(matches("label", r.Label, keyLabelPattern), inRange("length", int64(r.Length), MinDerivedKey, MaxDerivedKey)); f != nil {
		return wireInvalid(objectKeyRequest, f)
	}
	return nil
}

// DerivedKey answers a key request.
type DerivedKey struct {
	Key []byte `json:"key"`
}

// DecodeDerivedKey decodes and validates a key answer.
func DecodeDerivedKey(data []byte) (*DerivedKey, error) {
	var k DerivedKey
	if err := decodeStrict(objectDerivedKey, data, &k); err != nil {
		return nil, err
	}
	if err := k.Validate(); err != nil {
		return nil, err
	}
	return &k, nil
}

// Validate checks the answer's rules.
func (k *DerivedKey) Validate() error {
	if f := inRange("key", int64(len(k.Key)), MinDerivedKey, MaxDerivedKey); f != nil {
		return wireInvalid(objectDerivedKey, f)
	}
	return nil
}

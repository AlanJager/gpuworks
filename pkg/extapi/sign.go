package extapi

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
)

// Signature algorithms. The algorithm belongs to the key, never to the
// signature: a reader verifies with the algorithm of the key it trusts under
// the signature's key id, so a signature cannot choose a weaker one.
const (
	// AlgEd25519: a 32-byte Ed25519 public key; 64-byte signatures over the
	// payload (RFC 8032, pure Ed25519).
	AlgEd25519 = "ed25519"
	// AlgECDSAP256: a NIST P-256 public key as an uncompressed SEC 1 point
	// (65 bytes, 0x04 || X || Y); ASN.1 DER signatures over the SHA-256 of
	// the payload, the form hardware keys and cloud key services return.
	// A signature must be DER exactly (minimal encoding, nothing after it)
	// with s at most n/2 ("low-S"): (r, n-s) also verifies, so without the
	// rule anyone could re-encode a signature into different bytes. Seal
	// normalizes what a signer returns.
	AlgECDSAP256 = "ecdsa-p256-sha256"
)

// Envelope limits. A reader refuses an encoded envelope longer than
// MaxEnvelopeBytes (the base64 of the largest payload plus the signatures),
// and a signed document (manifest, certificate, trust update, revocation
// list) longer than MaxEnvelopePayload, before parsing it.
const (
	MaxEnvelopePayload = 4 << 20
	MaxEnvelopeBytes   = 6 << 20
	MaxSignatures      = 8
	maxSignatureBytes  = 512
)

// checkSize refuses data longer than limit before it is parsed.
func checkSize(object string, data []byte, limit int) error {
	if len(data) > limit {
		return malformed(object, &fieldError{rule: RuleRange})
	}
	return nil
}

// Envelope is a signed document: the payload bytes exactly as signed, and
// the signatures over them. Keeping both in one file means a reader never
// pairs a document with a signature of another version of it. Several
// signatures let a signer rotate keys: during the overlap it signs with the
// old and the new key, and readers that trust either accept the document.
//
// It is the shape of the server's release channel envelope, so one reader
// opens both.
type Envelope struct {
	Payload    []byte      `json:"payload"`
	Signatures []Signature `json:"signatures"`
}

// Signature is one signature over an envelope's payload.
type Signature struct {
	// KeyID names the key that made it (see ValidKeyID). A key id names
	// exactly one key, forever: it is never reused for another key, so a
	// reader that trusts a key id trusts one key.
	KeyID string `json:"keyid"`
	Sig   []byte `json:"sig"`
}

// PublicKey is a public key a reader may trust, under its key id.
type PublicKey struct {
	KeyID string `json:"keyid"`
	Alg   string `json:"alg"`
	Key   []byte `json:"public_key"`
}

func (k PublicKey) validate(path string) *fieldError {
	if f := first(
		matches(joinPath(path, "keyid"), k.KeyID, keyIDPattern),
		oneOf(joinPath(path, "alg"), k.Alg, AlgEd25519, AlgECDSAP256),
	); f != nil {
		return f
	}
	if !k.wellFormed() {
		return &fieldError{field: joinPath(path, "public_key"), rule: RuleGrammar}
	}
	return nil
}

func (k PublicKey) wellFormed() bool {
	switch k.Alg {
	case AlgEd25519:
		return len(k.Key) == ed25519.PublicKeySize
	case AlgECDSAP256:
		_, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), k.Key)
		return err == nil
	}
	return false
}

// Verify reports whether sig is k's signature over msg.
func (k PublicKey) Verify(msg, sig []byte) bool {
	switch k.Alg {
	case AlgEd25519:
		return len(k.Key) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(k.Key), msg, sig)
	case AlgECDSAP256:
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), k.Key)
		if err != nil {
			return false
		}
		if _, ok := parseLowS(sig); !ok {
			return false
		}
		sum := sha256.Sum256(msg)
		return ecdsa.VerifyASN1(pub, sum[:], sig)
	}
	return false
}

type ecdsaSig struct{ R, S *big.Int }

var p256HalfOrder = new(big.Int).Rsh(elliptic.P256().Params().N, 1)

// parseLowS decodes a P-256 signature that is exactly DER, with r and s
// positive and s at most n/2.
func parseLowS(sig []byte) (ecdsaSig, bool) {
	var s ecdsaSig
	rest, err := asn1.Unmarshal(sig, &s)
	if err != nil || len(rest) != 0 || s.R.Sign() <= 0 || s.S.Sign() <= 0 || s.S.Cmp(p256HalfOrder) > 0 {
		return s, false
	}
	der, err := asn1.Marshal(s)
	return s, err == nil && bytes.Equal(der, sig)
}

// lowS returns sig, a valid DER P-256 signature, with s replaced by n-s
// when s is above n/2.
func lowS(sig []byte) ([]byte, error) {
	var s ecdsaSig
	rest, err := asn1.Unmarshal(sig, &s)
	if err != nil || len(rest) != 0 || s.R.Sign() <= 0 || s.S.Sign() <= 0 {
		return nil, errors.New("signer returned a malformed ECDSA signature")
	}
	if s.S.Cmp(p256HalfOrder) > 0 {
		s.S = new(big.Int).Sub(elliptic.P256().Params().N, s.S)
	}
	return asn1.Marshal(s)
}

// PublicKeyOf returns the wire form of an Ed25519 or ECDSA P-256 public key.
func PublicKeyOf(keyID string, pub crypto.PublicKey) (PublicKey, error) {
	switch p := pub.(type) {
	case ed25519.PublicKey:
		return PublicKey{KeyID: keyID, Alg: AlgEd25519, Key: append([]byte(nil), p...)}, nil
	case *ecdsa.PublicKey:
		if p.Curve != elliptic.P256() {
			return PublicKey{}, errors.New("ecdsa key is not on P-256")
		}
		raw, err := p.Bytes()
		if err != nil {
			return PublicKey{}, err
		}
		return PublicKey{KeyID: keyID, Alg: AlgECDSAP256, Key: raw}, nil
	}
	return PublicKey{}, fmt.Errorf("unsupported public key type %T", pub)
}

// KeySet is the keys a reader trusts, by key id.
type KeySet map[string]PublicKey

// NewKeySet builds a KeySet, refusing malformed keys and repeated key ids.
func NewKeySet(keys ...PublicKey) (KeySet, error) {
	set := KeySet{}
	for i, k := range keys {
		if f := k.validate(strconv.Itoa(i)); f != nil {
			return nil, wireInvalid("public_key", f)
		}
		if _, dup := set[k.KeyID]; dup {
			return nil, wireInvalid("public_key", &fieldError{field: joinPath(strconv.Itoa(i), "keyid"), rule: RuleDuplicate})
		}
		set[k.KeyID] = k
	}
	return set, nil
}

// KeySigner is a private key and the key id it signs under. Signer may be
// an ed25519.PrivateKey, an *ecdsa.PrivateKey on P-256, or any crypto.Signer
// backed by such a key (a hardware token, a cloud key service).
type KeySigner struct {
	KeyID  string
	Signer crypto.Signer
}

// Seal signs a canonical payload with every signer and returns the envelope.
func Seal(payload []byte, signers ...KeySigner) (*Envelope, error) {
	if !IsCanonical(payload) {
		return nil, newError(ErrorWireNotCanonical, map[string]any{"object": "envelope"}, "payload is not canonical JSON")
	}
	env := &Envelope{Payload: payload}
	for _, s := range signers {
		if !keyIDPattern.MatchString(s.KeyID) {
			return nil, fmt.Errorf("key id %q is not valid", s.KeyID)
		}
		var sig []byte
		var err error
		switch pub := s.Signer.Public().(type) {
		case ed25519.PublicKey:
			sig, err = s.Signer.Sign(rand.Reader, payload, crypto.Hash(0))
		case *ecdsa.PublicKey:
			if pub.Curve != elliptic.P256() {
				return nil, errors.New("ecdsa key is not on P-256")
			}
			sum := sha256.Sum256(payload)
			sig, err = s.Signer.Sign(rand.Reader, sum[:], crypto.SHA256)
			if err == nil {
				sig, err = lowS(sig)
			}
		default:
			return nil, fmt.Errorf("unsupported signer key type %T", pub)
		}
		if err != nil {
			return nil, err
		}
		env.Signatures = append(env.Signatures, Signature{KeyID: s.KeyID, Sig: sig})
	}
	if f := env.validate(); f != nil {
		return nil, wireInvalid(objectEnvelope, f)
	}
	return env, nil
}

const objectEnvelope = "envelope"

// DecodeEnvelope decodes and validates an envelope; it verifies nothing.
func DecodeEnvelope(data []byte) (*Envelope, error) {
	if err := checkSize(objectEnvelope, data, MaxEnvelopeBytes); err != nil {
		return nil, err
	}
	var env Envelope
	if err := decodeStrict(objectEnvelope, data, &env); err != nil {
		return nil, err
	}
	if f := env.validate(); f != nil {
		return nil, wireInvalid(objectEnvelope, f)
	}
	return &env, nil
}

func (e *Envelope) validate() *fieldError {
	if len(e.Payload) == 0 {
		return &fieldError{field: "payload", rule: RuleRequired}
	}
	if len(e.Payload) > MaxEnvelopePayload {
		return &fieldError{field: "payload", rule: RuleRange}
	}
	if len(e.Signatures) == 0 {
		return &fieldError{field: "signatures", rule: RuleRequired}
	}
	if len(e.Signatures) > MaxSignatures {
		return &fieldError{field: "signatures", rule: RuleRange}
	}
	seen := map[string]bool{}
	for i, s := range e.Signatures {
		at := joinPath("signatures", strconv.Itoa(i))
		if f := matches(joinPath(at, "keyid"), s.KeyID, keyIDPattern); f != nil {
			return f
		}
		if seen[s.KeyID] {
			return &fieldError{field: joinPath(at, "keyid"), rule: RuleDuplicate}
		}
		seen[s.KeyID] = true
		if len(s.Sig) == 0 || len(s.Sig) > maxSignatureBytes {
			return &fieldError{field: joinPath(at, "sig"), rule: RuleRange}
		}
	}
	return nil
}

// Verified is an envelope that passed Verify.
type Verified struct {
	// Payload is the signed bytes, in canonical form.
	Payload []byte
	// KeyIDs are the trusted keys whose signatures verified, sorted.
	KeyIDs []string
}

// Verify checks e against the trusted keys, for a document of kind object
// (used in error parameters). Signatures by keys outside trusted are
// skipped, so a signer can add a new key before every reader trusts it. A
// signature by a trusted key that does not verify fails the whole envelope:
// a key id names one key forever, so a failure is tampering or corruption,
// never a rotation. At least one trusted signature must verify, and the
// payload must be canonical JSON.
func (e *Envelope) Verify(object string, trusted KeySet) (*Verified, error) {
	if f := e.validate(); f != nil {
		return nil, wireInvalid(objectEnvelope, f)
	}
	var ids []string
	for _, s := range e.Signatures {
		key, ok := trusted[s.KeyID]
		if !ok {
			continue
		}
		if !key.Verify(e.Payload, s.Sig) {
			return nil, newError(ErrorSignatureInvalid, map[string]any{"object": object, "keyid": s.KeyID},
				"%s: signature by %s does not verify", object, s.KeyID)
		}
		ids = append(ids, s.KeyID)
	}
	if len(ids) == 0 {
		return nil, newError(ErrorSignatureUntrusted, map[string]any{"object": object},
			"%s: no signature by a trusted key", object)
	}
	if !IsCanonical(e.Payload) {
		return nil, newError(ErrorWireNotCanonical, map[string]any{"object": object},
			"%s: signed payload is not canonical JSON", object)
	}
	sort.Strings(ids)
	return &Verified{Payload: e.Payload, KeyIDs: ids}, nil
}

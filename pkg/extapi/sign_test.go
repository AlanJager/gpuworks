package extapi

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Keys are generated per test run: no private key is ever checked in.

func newEd25519(t *testing.T, keyID string) (KeySigner, PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := PublicKeyOf(keyID, pub)
	if err != nil {
		t.Fatal(err)
	}
	return KeySigner{KeyID: keyID, Signer: priv}, pk
}

func newP256(t *testing.T, keyID string) (KeySigner, PublicKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := PublicKeyOf(keyID, &priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return KeySigner{KeyID: keyID, Signer: priv}, pk
}

func keySet(t *testing.T, keys ...PublicKey) KeySet {
	t.Helper()
	set, err := NewKeySet(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func roundTripEnvelope(t *testing.T, env *Envelope) *Envelope {
	t.Helper()
	raw, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestSealAndVerifyBothAlgorithms(t *testing.T) {
	ed, edPub := newEd25519(t, "gwroot-1")
	ec, ecPub := newP256(t, "gwroot-2")
	payload := []byte(`{"format":1,"serial":3}`)
	env, err := Seal(payload, ed, ec)
	if err != nil {
		t.Fatal(err)
	}
	env = roundTripEnvelope(t, env)
	for name, trusted := range map[string]KeySet{
		"ed25519 only": keySet(t, edPub),
		"p256 only":    keySet(t, ecPub),
		"both":         keySet(t, edPub, ecPub),
	} {
		v, err := env.Verify("trust_update", trusted)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(v.Payload) != string(payload) || len(v.KeyIDs) != len(trusted) {
			t.Fatalf("%s: verified %+v", name, v)
		}
	}
}

func TestVerifyRefusals(t *testing.T) {
	ed, edPub := newEd25519(t, "gwroot-1")
	_, otherPub := newEd25519(t, "gwroot-9")
	payload := []byte(`{"a":1}`)
	env, err := Seal(payload, ed)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.Verify("x", keySet(t, otherPub)); CodeOf(err) != ErrorSignatureUntrusted {
		t.Fatalf("untrusted signer: %v", err)
	}

	tampered := *env
	tampered.Payload = []byte(`{"a":2}`)
	if _, err := tampered.Verify("x", keySet(t, edPub)); CodeOf(err) != ErrorSignatureInvalid {
		t.Fatalf("tampered payload: %v", err)
	}

	// The same key id bound to another key: a key id names one key, so
	// a failing trusted signature fails the envelope even beside a good one.
	stale := PublicKey{KeyID: "gwroot-1", Alg: AlgEd25519, Key: otherPub.Key}
	if _, err := env.Verify("x", keySet(t, stale)); CodeOf(err) != ErrorSignatureInvalid {
		t.Fatalf("wrong key under a trusted id: %v", err)
	}
	ed2, ed2Pub := newEd25519(t, "gwroot-2")
	both, err := Seal(payload, ed, ed2)
	if err != nil {
		t.Fatal(err)
	}
	both.Signatures[0].Sig[0] ^= 0xff
	if _, err := both.Verify("x", keySet(t, edPub, ed2Pub)); CodeOf(err) != ErrorSignatureInvalid {
		t.Fatalf("one bad trusted signature beside a good one: %v", err)
	}

	// A signature over a non-canonical payload never verifies as a document.
	raw := []byte(`{"b":1, "a":2}`)
	sig := ed25519.Sign(ed.Signer.(ed25519.PrivateKey), raw)
	nc := &Envelope{Payload: raw, Signatures: []Signature{{KeyID: "gwroot-1", Sig: sig}}}
	if _, err := nc.Verify("x", keySet(t, edPub)); CodeOf(err) != ErrorWireNotCanonical {
		t.Fatalf("non-canonical payload: %v", err)
	}
	if _, err := Seal(raw, ed); CodeOf(err) != ErrorWireNotCanonical {
		t.Fatalf("seal of non-canonical payload: %v", err)
	}
}

func TestAlgorithmComesFromTheTrustedKey(t *testing.T) {
	ec, ecPub := newP256(t, "gwroot-2")
	env, err := Seal([]byte(`{"a":1}`), ec)
	if err != nil {
		t.Fatal(err)
	}
	// A reader that trusts root-2 as an Ed25519 key never verifies the
	// ECDSA signature: the signature cannot pick the algorithm.
	asEd := PublicKey{KeyID: "gwroot-2", Alg: AlgEd25519, Key: make([]byte, ed25519.PublicKeySize)}
	if _, err := env.Verify("x", keySet(t, asEd)); CodeOf(err) != ErrorSignatureInvalid {
		t.Fatalf("algorithm confusion: %v", err)
	}
	if _, err := env.Verify("x", keySet(t, ecPub)); err != nil {
		t.Fatal(err)
	}
}

func TestKeySetRefusesBadKeys(t *testing.T) {
	_, pub := newEd25519(t, "gwroot-1")
	if _, err := NewKeySet(pub, pub); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("duplicate key id: %v", err)
	}
	if _, err := NewKeySet(PublicKey{KeyID: "Root 1", Alg: AlgEd25519, Key: pub.Key}); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("bad key id: %v", err)
	}
	if _, err := NewKeySet(PublicKey{KeyID: "gwroot-1", Alg: AlgECDSAP256, Key: pub.Key}); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("ed25519 bytes as a P-256 point: %v", err)
	}
	if _, err := PublicKeyOf("x1", "not a key"); err == nil {
		t.Fatal("unsupported key type accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := PublicKeyOf("x1", &p384.PublicKey); err == nil {
		t.Fatal("P-384 accepted")
	}
	if _, err := Seal([]byte(`{}`), KeySigner{KeyID: "x1", Signer: p384}); err == nil {
		t.Fatal("P-384 signer accepted")
	}
	if _, err := Seal([]byte(`{}`), KeySigner{KeyID: "BAD", Signer: p384}); err == nil {
		t.Fatal("bad key id accepted")
	}
}

// A publisher certificate, signed by a root, opened from publisher.crt,
// then the manifest it signs: the chain a verifier walks.
func TestCertificateChainOpens(t *testing.T) {
	root, rootPub := newEd25519(t, "gwroot-1")
	pubSigner, pubKey := newP256(t, "acme-2026a")
	cert := PublisherCert{
		Format: 1, Serial: 7, Publisher: "acme", KeyID: pubKey.KeyID, Alg: pubKey.Alg, PublicKey: pubKey.Key,
		NotBefore: NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		NotAfter:  NewTime(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
		Grants:    CertGrants{IDPrefixes: []string{"acme_"}, MinIsolation: IsolationLandlock},
	}
	certPayload, err := Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	certEnv, err := Seal(certPayload, root)
	if err != nil {
		t.Fatal(err)
	}
	bundleRaw, err := json.Marshal([]*Envelope{certEnv})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := DecodeCertBundle(bundleRaw)
	if err != nil {
		t.Fatal(err)
	}
	got, issuers, err := OpenPublisherCert(&bundle[0], keySet(t, rootPub))
	if err != nil {
		t.Fatal(err)
	}
	if got.Publisher != "acme" || len(issuers) != 1 || issuers[0] != "gwroot-1" || !got.CoversID("acme_backup") || got.CoversID("other") {
		t.Fatalf("opened %+v by %v", got, issuers)
	}

	manifest, err := Marshal(Manifest{
		Schema: 1, ID: "acme_backup", Publisher: "acme", Version: "1.0.0",
		Created: NewTime(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)), Protocol: Range{Min: 1, Max: 1},
		Files: []FileEntry{{Path: "bin/linux_amd64/x", Size: 1, SHA256: sha256Hex([]byte("x")), Exec: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Seal(manifest, pubSigner)
	if err != nil {
		t.Fatal(err)
	}
	v, err := sig.Verify("manifest", keySet(t, got.Key()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(v.Payload); err != nil {
		t.Fatal(err)
	}
	if DigestOf(v.Payload) != DigestOf(manifest) {
		t.Fatal("digest of the verified payload differs")
	}

	if _, err := DecodeCertBundle([]byte(`[]`)); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("empty bundle: %v", err)
	}
}

func TestTrustUpdateOpensOnlyUnderATrustedRoot(t *testing.T) {
	root, rootPub := newEd25519(t, "gwroot-1")
	_, newRoot := newP256(t, "gwroot-3")
	u := TrustUpdate{Format: 1, Serial: 2, Created: NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
		AddRoots: []PublicKey{newRoot}, RetireRoots: []string{"gwroot-0"}}
	raw, err := Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Seal(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenTrustUpdate(env, keySet(t, rootPub))
	if err != nil {
		t.Fatal(err)
	}
	if got.AddRoots[0].KeyID != "gwroot-3" {
		t.Fatalf("%+v", got)
	}
	if _, err := OpenTrustUpdate(env, keySet(t, newRoot)); !errors.Is(err, &Error{Code: ErrorSignatureUntrusted}) {
		t.Fatalf("update opened under a root it adds: %v", err)
	}
}

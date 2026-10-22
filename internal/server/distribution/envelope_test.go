package distribution

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	payload := []byte(`{"schema":1}`)
	raw, err := Seal(payload, testKeyID, testPrivate)
	if err != nil {
		t.Fatal(err)
	}
	// Literal wire keys: CI writes them, released clients read them.
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["payload"] != base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("payload on the wire = %v", wire["payload"])
	}
	sig := wire["signatures"].([]any)[0].(map[string]any)
	if sig["keyid"] != testKeyID || sig["sig"] == "" {
		t.Fatalf("signature on the wire = %v", sig)
	}
	got, keyID, err := Open(raw, testKeys())
	if err != nil || string(got) != string(payload) || keyID != testKeyID {
		t.Fatalf("Open = %q, %q, %v", got, keyID, err)
	}
}

func TestOpenRefusesTamperingAndUnknownKeys(t *testing.T) {
	raw, err := Seal([]byte(`{"schema":1}`), testKeyID, testPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Payload = []byte(`{"schema":1,"channel":"evil"}`)
	tampered, _ := json.Marshal(envelope)
	if _, _, err := Open(tampered, testKeys()); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("tampered payload: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := Open(raw, Keys{testKeyID: other}); err == nil {
		t.Fatal("a signature checked against the wrong key must not verify")
	}
	if _, _, err := Open(raw, Keys{"someone-else": testPublic}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("unknown key id: %v", err)
	}
	for name, bad := range map[string]string{
		"not json":   "nope",
		"no payload": `{"signatures":[]}`,
	} {
		if _, _, err := Open([]byte(bad), testKeys()); err == nil {
			t.Errorf("%s: Open should fail", name)
		}
	}
}

// During a key rotation an index carries the new and the old signature; a
// client that knows either key must accept it, and a broken signature by one
// trusted key must not hide a good one by another.
func TestOpenDuringRotation(t *testing.T) {
	payload := []byte(`{"schema":1}`)
	newPublic, newPrivate, _ := ed25519.GenerateKey(rand.Reader)
	envelope := Envelope{Payload: payload, Signatures: []Signature{
		{KeyID: "new", Sig: ed25519.Sign(newPrivate, payload)},
		{KeyID: testKeyID, Sig: ed25519.Sign(testPrivate, payload)},
	}}
	raw, _ := json.Marshal(envelope)
	if _, keyID, err := Open(raw, testKeys()); err != nil || keyID != testKeyID {
		t.Fatalf("old client: %q %v", keyID, err)
	}
	if _, keyID, err := Open(raw, Keys{"new": newPublic}); err != nil || keyID != "new" {
		t.Fatalf("new client: %q %v", keyID, err)
	}
	envelope.Signatures[0].Sig = []byte("garbage")
	raw, _ = json.Marshal(envelope)
	if _, keyID, err := Open(raw, Keys{"new": newPublic, testKeyID: testPublic}); err != nil || keyID != testKeyID {
		t.Fatalf("one bad signature among good ones: %q %v", keyID, err)
	}
}

func TestSealRefusesBadKeys(t *testing.T) {
	if _, err := Seal([]byte("x"), "", testPrivate); err == nil {
		t.Fatal("a key id is required")
	}
	if _, err := Seal([]byte("x"), "k", ed25519.PrivateKey("short")); err == nil {
		t.Fatal("a non-ed25519 key must be refused")
	}
}

func TestKeyDecoding(t *testing.T) {
	seed := testPrivate.Seed()
	key, err := PrivateKeyFromSeed(" " + base64.StdEncoding.EncodeToString(seed) + "\n")
	if err != nil || !key.Equal(testPrivate) {
		t.Fatalf("PrivateKeyFromSeed = %v", err)
	}
	if _, err := PrivateKeyFromSeed("!!"); err == nil {
		t.Fatal("bad base64 must fail")
	}
	if _, err := PrivateKeyFromSeed(base64.StdEncoding.EncodeToString(seed[:16])); err == nil {
		t.Fatal("a short seed must fail")
	}
	public, err := PublicKey(base64.StdEncoding.EncodeToString(testPublic))
	if err != nil || !public.Equal(testPublic) {
		t.Fatalf("PublicKey = %v", err)
	}
	if _, err := PublicKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("a short public key must fail")
	}
	if _, err := PublicKey("%%"); err == nil {
		t.Fatal("bad base64 must fail")
	}
}

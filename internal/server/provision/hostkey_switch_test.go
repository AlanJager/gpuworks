package provision

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Host key checking is off (T77): an address a cloud provider gave to a new
// instance is accepted with its new key, and nothing is pinned for later.
// Turned on, the trust-on-first-use check still rejects the changed key.
func TestHostKeyCheckIsOffAndPinsNothing(t *testing.T) {
	first, second := testHostKey(t), testHostKey(t)
	store := newMemoryHostKeyStore()
	const endpoint = "47.102.121.172:22"
	for _, key := range []ssh.PublicKey{first, second} {
		if err := hostKeyCallback(store, endpoint)(endpoint, nil, key); err != nil {
			t.Fatalf("checking is off, yet a key was refused: %v", err)
		}
	}
	if _, pinned := store.Get(endpoint); pinned {
		t.Fatal("a key was recorded while checking is off")
	}

	verifyHostKeys = true
	t.Cleanup(func() { verifyHostKeys = false })
	if err := hostKeyCallback(store, endpoint)(endpoint, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := hostKeyCallback(store, endpoint)(endpoint, nil, second); err == nil {
		t.Fatal("with checking on, a changed key must be refused")
	}
}

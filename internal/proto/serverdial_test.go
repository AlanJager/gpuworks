package proto

import "testing"

func TestServerDialHMAC(t *testing.T) {
	proof := ServerDialHMAC("secret", "nonce")
	if proof == "" {
		t.Fatal("proof is empty")
	}
	if !VerifyServerDialHMAC("secret", "nonce", proof) {
		t.Fatal("proof should verify")
	}
	if VerifyServerDialHMAC("other", "nonce", proof) {
		t.Fatal("wrong secret should not verify")
	}
	if VerifyServerDialHMAC("secret", "other", proof) {
		t.Fatal("wrong nonce should not verify")
	}
}

func TestServerDialNodeSecret(t *testing.T) {
	const want = "1e16c337973f9ad03ebb4df8c7d71ded6a5b12f4c89a3f8866d33efc62f0c54b"
	if got := ServerDialNodeSecret("node-token"); got != want {
		t.Fatalf("ServerDialNodeSecret = %q, want %q", got, want)
	}
}

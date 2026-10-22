package auth

import (
	"strings"
	"testing"
	"time"
)

func TestSecretboxRoundTripAndTamperDetection(t *testing.T) {
	box, err := NewSecretbox("master-secret")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("cloud-api-key"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := box.Decrypt(ciphertext)
	if err != nil || string(plaintext) != "cloud-api-key" {
		t.Fatalf("decrypt = %q, err=%v", plaintext, err)
	}
	if _, err := box.Decrypt(ciphertext[:len(ciphertext)-1] + "x"); err == nil {
		t.Fatal("tampered ciphertext unexpectedly decrypted")
	}
	if _, err := NewSecretbox(""); err == nil {
		t.Fatal("empty master secret unexpectedly accepted")
	}
}

func TestPasswordHashRoundTripAndMalformedHashRejection(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash format = %q", hash)
	}
	if !VerifyPassword("correct horse battery staple", hash) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", hash) {
		t.Fatal("wrong password accepted")
	}
	for _, malformed := range []string{"", "argon2id", "$argon2i$v=19$m=1,t=1,p=1$$", "$argon2id$v=19$m=1,t=1,p=1$%%%$%%%"} {
		if VerifyPassword("correct horse battery staple", malformed) {
			t.Errorf("malformed hash accepted: %q", malformed)
		}
	}
}

func TestAPIKeyGenerationPrefixAndBearerParsing(t *testing.T) {
	full, prefix := NewAPIKey()
	if !strings.HasPrefix(full, "gw-") || prefix != PrefixOf(full) || len(prefix) != APIKeyPrefixLen {
		t.Fatalf("API key full=%q prefix=%q", full, prefix)
	}
	if PrefixOf("short") != "short" {
		t.Fatalf("short prefix = %q", PrefixOf("short"))
	}
	for header, want := range map[string]string{
		"Bearer " + full:          "" + full,
		"Bearer   " + full + "  ": full,
		full:                      full,
		"":                        "",
	} {
		if got := BearerToken(header); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
	if got := BearerOrCookie("", "cookie-token"); got != "cookie-token" {
		t.Fatalf("cookie token = %q", got)
	}
	if got := BearerOrCookie("Bearer header-token", "cookie-token"); got != "header-token" {
		t.Fatalf("header token = %q", got)
	}
}

func TestTokenSignerRejectsMalformedClaimsAndUsesDefaultTTL(t *testing.T) {
	signer, err := NewTokenSigner("secret", 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(Claims{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Verify("not-a-token"); err != ErrInvalidToken {
		t.Fatalf("malformed token error = %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token parts = %d", len(parts))
	}
	// A valid MAC over a payload missing the required subject is still invalid.
	badPayload := encodeSegment([]byte(`{"exp":9999999999}`))
	body := parts[0] + "." + badPayload
	forged := body + "." + signer.mac(body)
	if _, err := signer.Verify(forged); err != ErrInvalidToken {
		t.Fatalf("missing subject token error = %v", err)
	}
	if _, err := NewTokenSigner("", time.Hour); err == nil {
		t.Fatal("empty signing secret accepted")
	}
}

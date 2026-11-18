package auth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTokenSigner(t *testing.T) {
	s, err := NewTokenSigner("secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Sign(Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != 1 || got.Username != "admin" {
		t.Fatalf("claims = %+v", got)
	}
	if _, err := s.Verify(tok + "x"); err != ErrInvalidToken {
		t.Fatalf("tampered token err = %v", err)
	}
}

func TestTokenSignerExpiry(t *testing.T) {
	s, err := NewTokenSigner("secret", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Sign(Claims{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := s.Verify(tok); err != ErrExpiredToken {
		t.Fatalf("expiry err = %v", err)
	}
}

// The break-glass flag never travels in a token: signing leaves it out, and
// a validly signed token that claims it, under any spelling, reads false.
func TestTokensNeverCarryTheLocalAdministratorFlag(t *testing.T) {
	s, err := NewTokenSigner("local-admin-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Sign(Claims{UserID: 1, Username: "admin", Role: "admin", SessionID: "h", LocalAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if err != nil || strings.Contains(strings.ToLower(string(payload)), "local") {
		t.Fatalf("signed payload %s carries the flag (%v)", payload, err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	for _, key := range []string{"local_admin", "LocalAdmin", "localAdmin"} {
		body := encodeSegment([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
			encodeSegment([]byte(fmt.Sprintf(`{"sub":1,"username":"admin","role":"admin","sid":"h","exp":%d,%q:true}`, exp, key)))
		c, err := s.Verify(body + "." + s.mac(body))
		if err != nil {
			t.Fatalf("%s: a validly signed token was refused: %v", key, err)
		}
		if c.LocalAdmin {
			t.Fatalf("a token claiming %s made a local administrator", key)
		}
	}
}

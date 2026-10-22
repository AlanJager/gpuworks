package auth

import (
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

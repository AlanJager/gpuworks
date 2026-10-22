package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("expired token")
)

// Claims identifies an authenticated management user.
type Claims struct {
	UserID    uint   `json:"sub"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	SessionID string `json:"sid,omitempty"`
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

// TokenSigner creates and validates compact HMAC-SHA256 bearer tokens.
type TokenSigner struct {
	key []byte
	ttl time.Duration
}

func NewTokenSigner(secret string, ttl time.Duration) (*TokenSigner, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("empty token signing secret")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &TokenSigner{key: []byte(secret), ttl: ttl}, nil
}

func (s *TokenSigner) Sign(c Claims) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", ErrInvalidToken
	}
	now := time.Now().Unix()
	c.IssuedAt = now
	c.Expires = time.Now().Add(s.ttl).Unix()
	header := encodeSegment([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := header + "." + encodeSegment(payload)
	return body + "." + s.mac(body), nil
}

func (s *TokenSigner) Verify(token string) (Claims, error) {
	var c Claims
	if s == nil || len(s.key) == 0 {
		return c, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return c, ErrInvalidToken
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(parts[0]+"."+parts[1]))) {
		return c, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil || c.UserID == 0 || c.Expires == 0 {
		return c, ErrInvalidToken
	}
	if time.Now().Unix() >= c.Expires {
		return c, ErrExpiredToken
	}
	return c, nil
}

func (s *TokenSigner) mac(body string) string {
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func encodeSegment(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// BearerOrCookie extracts a management token from Authorization or a cookie.
func BearerOrCookie(header, cookie string) string {
	if t := BearerToken(header); t != "" {
		return t
	}
	return cookie
}

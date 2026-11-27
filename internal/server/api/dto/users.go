package dto

import "time"

// User is an account as an administrator manages it. The password hash is
// never part of any DTO.
type User struct {
	ID         uint       `json:"id"`
	Username   string     `json:"username"`
	Role       string     `json:"role"`        // admin | user
	LocalAdmin bool       `json:"local_admin"` // the floor of ADR 0071 decision 11
	Disabled   bool       `json:"disabled"`
	DisabledAt *time.Time `json:"disabled_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ExternalIdentity is one link from an account to an outside identity source
// (ADR 0085).
type ExternalIdentity struct {
	ID           uint       `json:"id"`
	Source       string     `json:"source"`
	Subject      string     `json:"subject"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSignInAt *time.Time `json:"last_sign_in_at"`
}

// ExternalIdentityRequest links an account to (source, subject).
type ExternalIdentityRequest struct {
	Source  string `json:"source"`
	Subject string `json:"subject"`
}

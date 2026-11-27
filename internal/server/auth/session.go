package auth

import "time"

// SessionPolicy bounds inactivity and the total lifetime of a management login.
type SessionPolicy struct {
	IdleTimeout time.Duration
	MaxLifetime time.Duration
	// Proxies may supply the browser origin through forwarded host and
	// protocol headers; nil trusts nobody.
	Proxies *ProxyTrust
	// Cookie names the management session cookie; empty is the default.
	Cookie string
	// LocalPassword says who may sign in with a local password: LocalPasswordAll
	// (the default) or LocalPasswordLocalAdmin, where only the local
	// administrators may (ADR 0085 decision 8). It travels with the session
	// policy because it is the same sign-in policy.
	LocalPassword string
}

// Who may sign in with a local password.
const (
	LocalPasswordAll        = "all"
	LocalPasswordLocalAdmin = "local_admin"
)

// Normalized supplies safe defaults for callers that do not configure sessions.
func (p SessionPolicy) Normalized() SessionPolicy {
	if p.LocalPassword == "" {
		p.LocalPassword = LocalPasswordAll
	}
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 24 * time.Hour
	}
	if p.MaxLifetime <= 0 {
		p.MaxLifetime = 7 * 24 * time.Hour
	}
	return p
}

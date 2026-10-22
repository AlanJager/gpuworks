package auth

import "time"

// SessionPolicy bounds inactivity and the total lifetime of a management login.
type SessionPolicy struct {
	IdleTimeout time.Duration
	MaxLifetime time.Duration
	// Proxies may supply the browser origin through forwarded host and
	// protocol headers; nil trusts nobody.
	Proxies *ProxyTrust
}

// Normalized supplies safe defaults for callers that do not configure sessions.
func (p SessionPolicy) Normalized() SessionPolicy {
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 24 * time.Hour
	}
	if p.MaxLifetime <= 0 {
		p.MaxLifetime = 7 * 24 * time.Hour
	}
	return p
}

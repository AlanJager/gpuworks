package identity

import (
	"crypto/tls"
	"errors"
	"time"
)

// PolicyAuthorizer binds an immutable authorization snapshot to this pinned
// authority without opening a file or contacting the database.
func (t *Trust) PolicyAuthorizer(policy TransportPolicy) (PeerAuthorizer, error) {
	if t == nil || t.certificate == nil {
		return nil, errors.New("transport policy requires pinned cluster trust")
	}
	return policy.Authorizer(t.clusterID, digest(t.certificate.Raw))
}

// TLSConfigsFromPolicy binds the durable authorization snapshot to both the
// pinned CA and the local member key before any listener or dial is started.
// The returned configurations retain an immutable snapshot until restart.
func (t *Trust) TLSConfigsFromPolicy(path string, certificate, key []byte, now time.Time) (*tls.Config, *tls.Config, error) {
	policy, err := LoadTransportPolicy(path)
	if err != nil {
		return nil, nil, err
	}
	authorize, err := policy.Authorizer(t.clusterID, digest(t.certificate.Raw))
	if err != nil {
		return nil, nil, err
	}
	local, err := parseCertificate(certificate)
	if err != nil {
		return nil, nil, err
	}
	fingerprint, err := t.VerifyMember(local, now)
	if err != nil {
		return nil, nil, err
	}
	if err := authorize(fingerprint); err != nil {
		return nil, nil, errors.New("local member is not authorized by persisted transport policy")
	}
	return t.TLSConfigs(certificate, key, now, authorize)
}

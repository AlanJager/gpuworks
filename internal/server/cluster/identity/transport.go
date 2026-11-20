package identity

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"time"
)

// Trust contains one pinned cluster authority, never the system trust store.
type Trust struct {
	certificate *x509.Certificate
	clusterID   string
	roots       *x509.CertPool
}

// CACertificatePEM returns the pinned authority for the database process,
// which runs outside this one and cannot share an in-memory tls.Config.
func (t *Trust) CACertificatePEM() []byte {
	if t == nil || t.certificate == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: t.certificate.Raw})
}

// MatchesIdentity binds a verified member certificate to the replicated
// cluster identity before its public key is recorded for another purpose.
func (t *Trust) MatchesIdentity(clusterID, caFingerprint string) bool {
	return t != nil && t.certificate != nil && t.clusterID == clusterID && digest(t.certificate.Raw) == caFingerprint
}

func NewTrust(certificatePEM []byte, clusterID, fingerprint string, now time.Time) (*Trust, error) {
	if !validID(clusterID) {
		return nil, errors.New("invalid expected cluster identity")
	}
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	if digest(certificate.Raw) != fingerprint {
		return nil, errors.New("cluster authority fingerprint mismatch")
	}
	if !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("cluster authority cannot sign certificates")
	}
	if _, ok := certificate.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("cluster authority requires Ed25519")
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		return nil, errors.New("cluster authority must be self-signed")
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != identityURI(clusterID, "").String() {
		return nil, errors.New("cluster authority identity mismatch")
	}
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return nil, errors.New("cluster authority is not currently valid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &Trust{certificate: certificate, clusterID: clusterID, roots: roots}, nil
}

// PeerAuthorizer checks local transport authorization after certificate
// validation. It must be safe for concurrent calls and must not query the same
// Raft database whose transport is being established. Revocation also requires
// terminating existing connections; rejecting future handshakes is insufficient.
type PeerAuthorizer func(keyFingerprint string) error

// TLSConfigs requires an explicit authorization policy. Certificate validity
// alone does not imply that a previously admitted member remains authorized.
// The dialing config deliberately leaves ServerName empty so tls.Dialer uses
// the actual destination hostname/IP and verifies its certificate SAN.
func (t *Trust) TLSConfigs(certificatePEM, keyPEM []byte, now time.Time, authorize PeerAuthorizer) (*tls.Config, *tls.Config, error) {
	if authorize == nil {
		return nil, nil, errors.New("member transport authorization policy is required")
	}
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, nil, err
	}
	if _, err := t.VerifyMember(certificate, now); err != nil {
		return nil, nil, err
	}
	if _, err := parseKey(keyPEM); err != nil {
		return nil, nil, err
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, nil, errors.New("member certificate and private key mismatch")
	}
	verify := func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return errors.New("member TLS requires a verified certificate chain")
		}
		fingerprint, err := t.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			return err
		}
		return authorize(fingerprint)
	}
	base := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		RootCAs:                t.roots.Clone(),
		SessionTicketsDisabled: true, VerifyConnection: verify,
	}
	listen, dial := base.Clone(), base.Clone()
	listen.ClientAuth = tls.RequireAndVerifyClientCert
	listen.ClientCAs = t.roots.Clone()
	// go-dqlite's Raft proxy selects server mode when ClientCAs is non-nil.
	// The outbound config must not inherit the listening CA pool.
	return listen, dial, nil
}

// VerifyMember returns the authenticated public-key identity. It validates
// cryptographic identity only; callers must separately enforce admission and
// revocation, and TLS clients must additionally verify the destination hostname.
func (t *Trust) VerifyMember(certificate *x509.Certificate, now time.Time) (string, error) {
	if certificate == nil || certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage != x509.KeyUsageDigitalSignature {
		return "", errors.New("invalid member certificate privileges")
	}
	if _, ok := certificate.PublicKey.(ed25519.PublicKey); !ok {
		return "", errors.New("member certificate requires Ed25519")
	}
	if len(certificate.ExtKeyUsage) != 2 || len(certificate.UnknownExtKeyUsage) != 0 {
		return "", errors.New("member certificate must have client and server authentication usages")
	}
	client, server := false, false
	for _, usage := range certificate.ExtKeyUsage {
		client = client || usage == x509.ExtKeyUsageClientAuth
		server = server || usage == x509.ExtKeyUsageServerAuth
	}
	if !client || !server {
		return "", errors.New("invalid member certificate authentication usages")
	}
	fingerprint := digest(certificate.RawSubjectPublicKeyInfo)
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != identityURI(t.clusterID, fingerprint).String() {
		return "", errors.New("member certificate cluster or key identity mismatch")
	}
	hostCount := len(certificate.DNSNames) + len(certificate.IPAddresses)
	if hostCount < 1 || hostCount > 2 || len(certificate.EmailAddresses) != 0 {
		return "", errors.New("member certificate must identify one or two member hosts")
	}
	hosts := make(map[string]bool, hostCount)
	for _, name := range certificate.DNSNames {
		normalized, err := memberHost(net.JoinHostPort(name, "443"))
		if err != nil || normalized != name || hosts[normalized] {
			return "", errors.New("invalid member certificate DNS host")
		}
		hosts[normalized] = true
	}
	for _, ip := range certificate.IPAddresses {
		normalized, err := memberHost(net.JoinHostPort(ip.String(), "443"))
		if err != nil || normalized != ip.String() || hosts[normalized] {
			return "", errors.New("invalid member certificate IP host")
		}
		hosts[normalized] = true
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: t.roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", errors.New("member certificate chain or validity rejected")
	}
	return fingerprint, nil
}

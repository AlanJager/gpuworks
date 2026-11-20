package identity

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// NormalizeMemberAddress uses the same host policy as member certificates.
func NormalizeMemberAddress(address string) (string, error) {
	host, err := memberHost(address)
	if err != nil {
		return "", err
	}
	_, port, _ := net.SplitHostPort(address)
	number, _ := strconv.Atoi(port)
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

// EnrollmentTLSConfig authenticates the initial HTTPS endpoint using the CA
// fingerprint supplied by the administrator. It is not member/Raft transport:
// joining hosts do not yet have a certificate or a locally installed CA.
// The server must send its leaf and pinned root, as the enrollment listener does.
func EnrollmentTLSConfig(clusterID, fingerprint, hostname string) (*tls.Config, error) {
	pin, err := hex.DecodeString(fingerprint)
	if !validID(clusterID) || err != nil || len(pin) != 32 || strings.ToLower(fingerprint) != fingerprint {
		return nil, errors.New("enrollment requires an explicit cluster identity and SHA256 authority pin")
	}
	host, err := memberHost(net.JoinHostPort(hostname, "443"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: host,
		// Verification is replaced, not omitted: the pinned CA is learned
		// from this handshake and is never accepted via the system root store.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 2 {
				return errors.New("enrollment endpoint must present its leaf and pinned authority")
			}
			root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: state.PeerCertificates[1].Raw})
			now := time.Now().UTC()
			trust, err := NewTrust(root, clusterID, fingerprint, now)
			if err != nil {
				return err
			}
			leaf := state.PeerCertificates[0]
			if _, err := trust.VerifyMember(leaf, now); err != nil {
				return err
			}
			return leaf.VerifyHostname(host)
		},
	}, nil
}

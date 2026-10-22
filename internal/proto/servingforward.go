package proto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// The serving forwarder lets the gateway reach an instance over the node's
// serving network instead of the agent tunnel. The agent listens with TLS on
// ServingForwardPort; after the handshake the gateway sends one preamble line
// "GPUWORKS-FWD/1 <instance uuid> <token>\n", the agent answers "OK\n" and
// splices the connection to the instance's loopback port.
//
// Both secrets derive from the node token, which only the server and that
// node hold: the per-instance token is HMAC(node token, instance uuid), and the
// TLS key is an ed25519 key seeded from HMAC(node token, fixed label). Nothing
// is stored or distributed, and regenerating a node token rotates both. The
// HMAC key is the raw node token, never ServerDialNodeSecret: server_dial
// signs agent-chosen nonces with that secret, so sharing it would turn the
// server_dial handshake into an oracle for these tokens.
const (
	ServingForwardPort  = 19443
	ServingForwardMagic = "GPUWORKS-FWD/1"
)

const servingForwardTLSLabel = "gpuworks serving-forward tls key v1"

// ServingForwardToken is the credential the gateway presents for one instance.
func ServingForwardToken(nodeToken, instanceUUID string) string {
	mac := hmac.New(sha256.New, []byte(nodeToken))
	mac.Write([]byte("instance:" + instanceUUID))
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidServingForwardToken compares in constant time.
func ValidServingForwardToken(nodeToken, instanceUUID, token string) bool {
	want := ServingForwardToken(nodeToken, instanceUUID)
	return hmac.Equal([]byte(want), []byte(token))
}

func servingForwardKey(nodeToken string) ed25519.PrivateKey {
	mac := hmac.New(sha256.New, []byte(nodeToken))
	mac.Write([]byte(servingForwardTLSLabel))
	return ed25519.NewKeyFromSeed(mac.Sum(nil))
}

// ServingForwardCertificate is the agent's TLS certificate: self-signed, with
// the node-token-derived key. Its other fields do not matter to the gateway,
// which pins the public key alone.
func ServingForwardCertificate(nodeToken string) (tls.Certificate, error) {
	key := servingForwardKey(nodeToken)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ServingForwardClientTLS pins the forwarder's derived public key. Hostname
// and CA checks are replaced by that pin, which is stronger for this purpose.
func ServingForwardClientTLS(nodeToken string) *tls.Config {
	want := servingForwardKey(nodeToken).Public().(ed25519.PublicKey)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verified by VerifyPeerCertificate below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("serving forwarder presented no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse serving forwarder certificate: %w", err)
			}
			got, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok || !bytes.Equal(got, want) {
				return errors.New("serving forwarder key does not match the node")
			}
			return nil
		},
	}
}

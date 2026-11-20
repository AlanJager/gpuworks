// Package identity binds cluster transport certificates to admitted public keys.
// It does not grant admission or persist credentials.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxMemberLifetime = 365 * 24 * time.Hour

// Authority is an explicitly loaded cluster signing authority. Keep its private
// material out of public enrollment responses, logs and resource previews.
type Authority struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
	clusterID   string
}

// VerifyIdentity binds a configured signer to the committed cluster identity
// and returns only its canonical public certificate, never signing material.
func (a *Authority) VerifyIdentity(clusterID, fingerprint string, now time.Time) ([]byte, error) {
	if a == nil || a.certificate == nil || a.clusterID != clusterID || digest(a.certificate.Raw) != fingerprint {
		return nil, errors.New("cluster signing authority identity mismatch")
	}
	if now.Before(a.certificate.NotBefore) || !now.Before(a.certificate.NotAfter) {
		return nil, errors.New("cluster authority is not currently valid")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.certificate.Raw}), nil
}

// CSRKeyFingerprint lets an enrollment service validate a CSR through its
// configured signer without taking ownership of private signing material.
func (a *Authority) CSRKeyFingerprint(csrPEM []byte) (string, error) {
	return CSRKeyFingerprint(csrPEM)
}

// NewAuthority creates candidate material; the caller must durably store it
// before publishing its identity. Loading errors must never call this function.
func NewAuthority(clusterID string, now time.Time) (certificatePEM, keyPEM []byte, err error) {
	if !validID(clusterID) {
		return nil, nil, errors.New("invalid cluster identity")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "GPUWorks cluster authority"},
		NotBefore: now.UTC().Add(-5 * time.Minute), NotAfter: now.UTC().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs:     []*url.URL{identityURI(clusterID, "")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(private)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, err
}

func LoadAuthority(certificatePEM, keyPEM []byte, clusterID, fingerprint string, now time.Time) (*Authority, error) {
	trust, err := NewTrust(certificatePEM, clusterID, fingerprint, now)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	public, ok := trust.certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !public.Equal(key.Public()) {
		return nil, errors.New("cluster authority private key mismatch")
	}
	return &Authority{certificate: trust.certificate, key: key, clusterID: clusterID}, nil
}

func NewMemberKey() ([]byte, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return encodeKey(key)
}

func NewMemberCSR(keyPEM []byte) ([]byte, error) {
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// CSRKeyFingerprint verifies the CSR signature and returns the SHA-256 digest
// of its DER SubjectPublicKeyInfo. A captured CSR is replayable: this method is
// not live authentication and must never authorize delivery of secret material.
func CSRKeyFingerprint(csrPEM []byte) (string, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return "", err
	}
	return digest(csr.RawSubjectPublicKeyInfo), nil
}

// IssueMember signs only an already admitted key/address binding. The caller
// must read that binding from committed admission state. CSR SANs/extensions
// are deliberately ignored, including requests for CA privileges.
func (a *Authority) IssueMember(csrPEM []byte, admittedKey, address string, now time.Time, lifetime time.Duration) ([]byte, error) {
	return a.IssueMemberWithAdditionalHost(csrPEM, admittedKey, address, "", now, lifetime)
}

// IssueMemberWithAdditionalHost adds only an independently authorized public
// hostname. Callers must obtain it from committed administrator policy, never
// from CSR SANs or a member-supplied request field.
func (a *Authority) IssueMemberWithAdditionalHost(csrPEM []byte, admittedKey, address, additionalHost string, now time.Time, lifetime time.Duration) ([]byte, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	if digest(csr.RawSubjectPublicKeyInfo) != admittedKey {
		return nil, errors.New("CSR differs from admitted member key")
	}
	if lifetime < time.Second || lifetime > MaxMemberLifetime {
		return nil, errors.New("invalid member certificate lifetime")
	}
	if now.Before(a.certificate.NotBefore) || !now.Before(a.certificate.NotAfter) {
		return nil, errors.New("cluster authority is not currently valid")
	}
	host, err := memberHost(address)
	if err != nil {
		return nil, err
	}
	if additionalHost != "" {
		additionalHost, err = memberHost(net.JoinHostPort(additionalHost, "443"))
		if err != nil {
			return nil, err
		}
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	start, end := now.UTC().Add(-5*time.Minute), now.UTC().Add(lifetime)
	if start.Before(a.certificate.NotBefore) {
		start = a.certificate.NotBefore
	}
	if end.After(a.certificate.NotAfter) {
		end = a.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "GPUWorks cluster member"},
		NotBefore: start, NotAfter: end, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:        []*url.URL{identityURI(a.clusterID, admittedKey)},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	if additionalHost != "" && additionalHost != host {
		if ip := net.ParseIP(additionalHost); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, additionalHost)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, csr.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func CertificateFingerprint(certificatePEM []byte) (string, error) {
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return "", err
	}
	return digest(certificate.Raw), nil
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, err := decodePEM(data, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseCSR(data []byte) (*x509.CertificateRequest, error) {
	block, err := decodePEM(data, "CERTIFICATE REQUEST")
	if err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if _, ok := csr.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("member CSR requires Ed25519")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, errors.New("invalid member CSR signature")
	}
	return csr, nil
}

func parseKey(data []byte) (ed25519.PrivateKey, error) {
	block, err := decodePEM(data, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid member private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("cluster private key requires Ed25519")
	}
	return key, nil
}

func encodeKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func decodePEM(data []byte, kind string) (*pem.Block, error) {
	if len(data) > 64<<10 {
		return nil, errors.New("cluster identity material exceeds size limit")
	}
	data = bytes.TrimSpace(data)
	if !bytes.HasPrefix(data, []byte("-----BEGIN "+kind+"-----")) {
		return nil, fmt.Errorf("expected one %s PEM block", kind)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != kind || len(block.Headers) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("expected one %s PEM block", kind)
	}
	return block, nil
}

func serialNumber() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func validID(id string) bool {
	value, err := hex.DecodeString(id)
	return err == nil && len(value) == 16 && strings.ToLower(id) == id
}
func identityURI(clusterID, key string) *url.URL {
	path := "/cluster/" + clusterID
	if key != "" {
		path += "/member/" + key
	}
	return &url.URL{Scheme: "spiffe", Host: "gpuworks", Path: path}
}

func memberHost(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("member address requires host and port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errors.New("invalid member port")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("member address must be unicast")
		}
		return ip.String(), nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if len(host) == 0 || len(host) > 253 {
		return "", errors.New("invalid member hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid member hostname")
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", errors.New("invalid member hostname")
			}
		}
	}
	return host, nil
}

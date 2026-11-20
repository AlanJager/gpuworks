package identity

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

type identityFixture struct {
	authority *Authority
	trust     *Trust
	ca, caKey []byte
	cluster   string
	now       time.Time
}

func newIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	f := identityFixture{cluster: strings.Repeat("a", 32), now: time.Now().UTC().Truncate(time.Second)}
	var err error
	f.ca, f.caKey, err = NewAuthority(f.cluster, f.now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := CertificateFingerprint(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	f.authority, err = LoadAuthority(f.ca, f.caKey, f.cluster, pin, f.now)
	if err != nil {
		t.Fatal(err)
	}
	f.trust, err = NewTrust(f.ca, f.cluster, pin, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f identityFixture) member(t *testing.T, address string) (cert, key []byte, fingerprint string) {
	t.Helper()
	key, err := NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err = CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = f.authority.IssueMember(csr, fingerprint, address, f.now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, fingerprint
}

func TestAuthorityPinClusterKeyAndValidity(t *testing.T) {
	f := newIdentityFixture(t)
	pin, _ := CertificateFingerprint(f.ca)
	other := newIdentityFixture(t)
	for _, tc := range []struct {
		name, cluster, pin string
		now                time.Time
	}{
		{"wrong pin", f.cluster, strings.Repeat("0", 64), f.now},
		{"wrong cluster", strings.Repeat("b", 32), pin, f.now},
		{"invalid cluster", "not-a-cluster", pin, f.now},
		{"not yet valid", f.cluster, pin, f.now.Add(-time.Hour)},
		{"expired", f.cluster, pin, f.now.AddDate(11, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTrust(f.ca, tc.cluster, tc.pin, tc.now); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
	if _, err := LoadAuthority(f.ca, other.caKey, f.cluster, pin, f.now); err == nil {
		t.Fatal("unrelated signing key accepted")
	}
	if _, _, err := NewAuthority("bad", f.now); err == nil {
		t.Fatal("invalid new cluster accepted")
	}
	for _, bad := range [][]byte{nil, append([]byte("unrelated-sensitive-prefix\n"), f.ca...), append(append([]byte{}, f.ca...), f.ca...), []byte(strings.Repeat("x", 65<<10))} {
		if _, err := CertificateFingerprint(bad); err == nil {
			t.Fatal("invalid certificate PEM accepted")
		}
	}
}

func TestMemberCSRRequestsCannotEscalateCertificate(t *testing.T) {
	f := newIdentityFixture(t)
	key, err := NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	private, err := parseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	constraints, err := asn1.Marshal(struct {
		CA      bool
		PathLen int
	}{true, 2})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "attacker authority"}, DNSNames: []string{"evil.example"}, IPAddresses: []net.IP{net.ParseIP("192.0.2.99")},
		URIs:            []*url.URL{identityURI(strings.Repeat("b", 32), "attacker")},
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: constraints}},
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	fp, err := CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := f.authority.IssueMember(csr, fp, "Member.Example.:9010", f.now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parseCertificate(issued)
	if err != nil {
		t.Fatal(err)
	}
	if cert.IsCA || cert.KeyUsage != x509.KeyUsageDigitalSignature || len(cert.DNSNames) != 1 || cert.DNSNames[0] != "member.example" || len(cert.IPAddresses) != 0 || len(cert.URIs) != 1 || cert.URIs[0].String() != identityURI(f.cluster, fp).String() || cert.Subject.CommonName == "attacker authority" {
		t.Fatalf("CSR privilege or identity escaped policy: %+v", cert)
	}
	if got, err := f.trust.VerifyMember(cert, f.now); err != nil || got != fp {
		t.Fatalf("issued member verification: %q %v", got, err)
	}
	dualPEM, err := f.authority.IssueMemberWithAdditionalHost(csr, fp, "Member.Example.:9010", "PUBLIC.example", f.now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dual, err := parseCertificate(dualPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(dual.DNSNames) != 2 || dual.VerifyHostname("member.example") != nil || dual.VerifyHostname("public.example") != nil || dual.VerifyHostname("evil.example") == nil || dual.VerifyHostname("192.0.2.99") == nil {
		t.Fatalf("additional host or CSR SAN escaped certificate policy: %+v", dual.DNSNames)
	}
	if got, err := f.trust.VerifyMember(dual, f.now); err != nil || got != fp {
		t.Fatalf("dual host member verification: %q %v", got, err)
	}
	_, otherKey, otherFP := f.member(t, "127.0.0.1:9011")
	if bytes.Equal(key, otherKey) || fp == otherFP {
		t.Fatal("members share private key or public identity")
	}
	if _, err := f.authority.IssueMember(csr, otherFP, "127.0.0.1:9010", f.now, time.Hour); err == nil {
		t.Fatal("CSR bound to another admitted key")
	}
	der[len(der)-1] ^= 1
	forged := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if _, err := CSRKeyFingerprint(forged); err == nil {
		t.Fatal("forged CSR signature accepted")
	}
	if _, err := f.authority.IssueMember(forged, fp, "127.0.0.1:9010", f.now, time.Hour); err == nil {
		t.Fatal("forged CSR issued")
	}
}

func TestMemberLifetimeAddressAndIdentityValidation(t *testing.T) {
	f := newIdentityFixture(t)
	certPEM, key, fp := f.member(t, "127.0.0.1:9010")
	csr, err := NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []time.Duration{0, time.Nanosecond, -time.Second, MaxMemberLifetime + time.Second} {
		if _, err := f.authority.IssueMember(csr, fp, "127.0.0.1:9010", f.now, ttl); err == nil {
			t.Fatalf("invalid lifetime %s", ttl)
		}
	}
	for _, addr := range []string{"missing-port", "0.0.0.0:9010", "[::]:9010", "224.0.0.1:9010", "host:0", "host:65536", "bad_host:9010"} {
		if _, err := f.authority.IssueMember(csr, fp, addr, f.now, time.Hour); err == nil {
			t.Fatalf("invalid address %s", addr)
		}
	}
	cert, err := parseCertificate(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.trust.VerifyMember(cert, cert.NotAfter.Add(time.Second)); err == nil {
		t.Fatal("expired member accepted")
	}
	if _, err := f.trust.VerifyMember(cert, cert.NotBefore.Add(-time.Second)); err == nil {
		t.Fatal("future member accepted")
	}
	for _, change := range []func(*x509.Certificate){
		func(c *x509.Certificate) { c.IsCA = true },
		func(c *x509.Certificate) { c.URIs = []*url.URL{identityURI(strings.Repeat("b", 32), fp)} },
		func(c *x509.Certificate) { c.URIs = []*url.URL{identityURI(f.cluster, strings.Repeat("f", 64))} },
		func(c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageCertSign },
		func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
	} {
		changed := *cert
		change(&changed)
		der, err := x509.CreateCertificate(rand.Reader, &changed, f.authority.certificate, cert.PublicKey, f.authority.key)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.trust.VerifyMember(signed, f.now); err == nil {
			t.Fatal("CA-signed incorrect member identity or privileges accepted")
		}
	}
	nearEnd := f.authority.certificate.NotAfter.Add(-time.Hour)
	clipped, err := f.authority.IssueMember(csr, fp, "127.0.0.1:9010", nearEnd, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseCertificate(clipped)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.NotAfter.Equal(f.authority.certificate.NotAfter) {
		t.Fatal("member lifetime exceeded authority")
	}
}

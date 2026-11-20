package identity

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"
)

func TestEnrollmentTLSAuthenticatesBeforeApplicationBytes(t *testing.T) {
	f := newIdentityFixture(t)
	pin, _ := CertificateFingerprint(f.ca)
	cert, key, _ := f.member(t, "127.0.0.1:9443")
	pair, err := tls.X509KeyPair(append(append([]byte{}, cert...), f.ca...), key)
	if err != nil {
		t.Fatal(err)
	}
	server := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	for _, scenario := range []string{"valid", "wrong pin", "wrong cluster", "wrong hostname", "missing root", "other root", "invalid signature", "expired leaf"} {
		t.Run(scenario, func(t *testing.T) {
			clusterID, expectedPin, hostname := f.cluster, pin, "127.0.0.1"
			listen := server.Clone()
			listen.Certificates = append([]tls.Certificate{}, server.Certificates...)
			listen.Certificates[0].Certificate = append([][]byte{}, pair.Certificate...)
			switch scenario {
			case "wrong pin":
				expectedPin = strings.Repeat("0", 64)
			case "wrong cluster":
				clusterID = strings.Repeat("b", 32)
			case "wrong hostname":
				hostname = "other.example"
			case "missing root":
				listen.Certificates[0].Certificate = pair.Certificate[:1]
			case "other root":
				other := newIdentityFixture(t)
				root, err := parseCertificate(other.ca)
				if err != nil {
					t.Fatal(err)
				}
				listen.Certificates[0].Certificate[1] = root.Raw
			case "invalid signature":
				changed := append([]byte{}, pair.Certificate[0]...)
				changed[len(changed)-1] ^= 1
				listen.Certificates[0].Certificate[0] = changed
			case "expired leaf":
				csr, err := NewMemberCSR(key)
				if err != nil {
					t.Fatal(err)
				}
				fp, err := CSRKeyFingerprint(csr)
				if err != nil {
					t.Fatal(err)
				}
				old, err := f.authority.IssueMember(csr, fp, "127.0.0.1:9443", f.now.Add(-time.Minute), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := parseCertificate(old)
				if err != nil {
					t.Fatal(err)
				}
				listen.Certificates[0].Certificate[0] = leaf.Raw
			}
			client, err := EnrollmentTLSConfig(clusterID, expectedPin, hostname)
			if err != nil {
				t.Fatal(err)
			}
			clientErr, serverErr := exchangeTLS(t, listen, client)
			if scenario == "valid" {
				if clientErr != nil || serverErr != nil {
					t.Fatalf("authenticated exchange: client=%v server=%v", clientErr, serverErr)
				}
			} else if clientErr == nil || serverErr == nil {
				t.Fatalf("untrusted peer accepted application bytes: client=%v server=%v", clientErr, serverErr)
			}
		})
	}
}

func TestEnrollmentTLSRequiresExplicitTrust(t *testing.T) {
	for _, input := range [][3]string{
		{"", strings.Repeat("a", 64), "localhost"},
		{strings.Repeat("a", 32), "", "localhost"},
		{strings.Repeat("a", 32), strings.Repeat("A", 64), "localhost"},
		{strings.Repeat("a", 32), strings.Repeat("a", 64), ""},
		{strings.Repeat("a", 32), strings.Repeat("a", 64), "0.0.0.0"},
	} {
		if _, err := EnrollmentTLSConfig(input[0], input[1], input[2]); err == nil {
			t.Fatal("missing or malformed trust accepted")
		}
	}
}

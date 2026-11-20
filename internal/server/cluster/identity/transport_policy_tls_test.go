package identity

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPersistentPolicyTLSRejectsUnknownAndRevokedKeys(t *testing.T) {
	f := newIdentityFixture(t)
	serverCert, serverKey, serverFP := f.member(t, "127.0.0.1:9010")
	clientCert, clientKey, clientFP := f.member(t, "127.0.0.1:9011")
	strangerCert, strangerKey, _ := f.member(t, "127.0.0.1:9012")
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "transport.json")
	pin, err := CertificateFingerprint(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{serverFP, clientFP}
	slices.Sort(keys)
	policy := TransportPolicy{ClusterID: f.cluster, CAFingerprint: pin, Revision: 1, AllowedKeys: keys, RevokedKeys: []string{}}
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	server, _, err := f.trust.TLSConfigsFromPolicy(path, serverCert, serverKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	_, client, err := f.trust.TLSConfigsFromPolicy(path, clientCert, clientKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if clientErr, serverErr := exchangeTLS(t, server, client); clientErr != nil || serverErr != nil {
		t.Fatalf("authorized exchange: client=%v server=%v", clientErr, serverErr)
	}
	// A CA-valid credential cannot admit its own public key by choosing a
	// permissive client-side policy. Authorization is checked at the receiving peer.
	_, stranger, err := f.trust.TLSConfigs(strangerCert, strangerKey, f.now, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, serverErr := exchangeTLS(t, server, stranger); serverErr == nil {
		t.Fatal("unknown CA-valid member admitted")
	}
	policy.Revision = 2
	policy.AllowedKeys = []string{serverFP}
	policy.RevokedKeys = []string{clientFP}
	if err := PublishTransportPolicy(path, 1, policy); err != nil {
		t.Fatal(err)
	}
	// Every exchange above has closed its listener/connections. Reload represents
	// explicit transport restart, not live revocation by editing a file.
	restarted, _, err := f.trust.TLSConfigsFromPolicy(path, serverCert, serverKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, serverErr := exchangeTLS(t, restarted, client); serverErr == nil {
		t.Fatal("revoked member admitted after restart")
	}
	if _, _, err := f.trust.TLSConfigsFromPolicy(path, clientCert, clientKey, f.now); err == nil {
		t.Fatal("revoked local identity started transport")
	}
	// The revoked member possesses CA signing material in this product. Even
	// signing a fresh certificate for its old key must not undo revocation.
	csr, err := NewMemberCSR(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	reissued, err := f.authority.IssueMember(csr, clientFP, "127.0.0.1:9011", f.now, MaxMemberLifetime)
	if err != nil {
		t.Fatal(err)
	}
	_, oldMember, err := f.trust.TLSConfigs(reissued, clientKey, f.now, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, serverErr := exchangeTLS(t, restarted, oldMember); serverErr == nil {
		t.Fatal("reissued revoked key bypassed persisted tombstone")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.trust.TLSConfigsFromPolicy(path, serverCert, serverKey, f.now); err == nil {
		t.Fatal("missing policy silently reconstructed")
	}
}

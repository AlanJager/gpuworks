package identity

import (
	"strings"
	"testing"
)

func TestTrustPolicyAuthorizerBindsAuthorityAndCopiesKeys(t *testing.T) {
	f := newIdentityFixture(t)
	_, _, key := f.member(t, "127.0.0.1:9010")
	pin, err := CertificateFingerprint(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	policy := TransportPolicy{ClusterID: f.cluster, CAFingerprint: pin, Revision: 1, AllowedKeys: []string{key}}
	authorize, err := f.trust.PolicyAuthorizer(policy)
	if err != nil || authorize(key) != nil {
		t.Fatalf("authorized member rejected: %v", err)
	}
	other := strings.Repeat("a", 64)
	policy.AllowedKeys[0] = other
	if authorize(key) != nil || authorize(other) == nil {
		t.Fatal("caller mutation changed captured authorization")
	}
	for _, change := range []func(*TransportPolicy){
		func(p *TransportPolicy) { p.ClusterID = strings.Repeat("b", 32) },
		func(p *TransportPolicy) { p.CAFingerprint = strings.Repeat("b", 64) },
		func(p *TransportPolicy) { p.Revision = 0 },
		func(p *TransportPolicy) { p.RevokedKeys = []string{other} },
	} {
		bad := policy
		change(&bad)
		if got, err := f.trust.PolicyAuthorizer(bad); err == nil || got != nil {
			t.Fatal("invalid authority or policy accepted")
		}
	}
	for _, trust := range []*Trust{nil, {}} {
		if got, err := trust.PolicyAuthorizer(policy); err == nil || got != nil {
			t.Fatal("missing pinned authority accepted")
		}
	}
}

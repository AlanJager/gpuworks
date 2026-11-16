package extapi

import (
	"testing"
	"time"
)

func testCert(publisher, keyID string, ids, ns []string) *PublisherCert {
	return &PublisherCert{
		Format: 1, Serial: 1, Publisher: publisher, KeyID: keyID, Alg: AlgEd25519,
		PublicKey: make([]byte, 32),
		NotBefore: NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		NotAfter:  NewTime(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
		Grants:    CertGrants{IDPrefixes: ids, FeatureNS: ns, MinIsolation: IsolationLandlock},
	}
}

// An entry ending in "_" is a prefix; any other entry is one exact id, so a
// grant never reaches past the "_" boundary into another publisher's ids.
func TestCertificateCoversIDsOnlyAtTheUnderscoreBoundary(t *testing.T) {
	c := testCert("acme", "acme-2026a", []string{"acme_", "acme_backup"}, nil)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{
		"acme_backup": true, "acme_reports": true, "acmebank_pay": false, "acme": false, "other": false,
	} {
		if got := c.CoversID(id); got != want {
			t.Errorf("CoversID(%q) = %v, want %v", id, got, want)
		}
	}
	exact := testCert("acme", "acme-2026a", []string{"acme_backup"}, nil)
	if !exact.CoversID("acme_backup") || exact.CoversID("acme_backup2") {
		t.Fatal("an entry without a trailing _ must cover exactly one id")
	}
}

// Only the project publisher may hold the bare namespace or ids outside
// "<publisher>_"; nobody may certify a key under a root's or another
// publisher's key id.
func TestCertificateGrantsStayInsideTheSubject(t *testing.T) {
	cases := []struct {
		name  string
		cert  *PublisherCert
		field string
	}{
		{"third party claims a bare id", testCert("acme", "acme-1", []string{"quota"}, nil), "grants.id_prefixes.0"},
		{"third party claims a longer publisher's ids", testCert("acme", "acme-1", []string{"acmebank_"}, nil), "grants.id_prefixes.0"},
		{"third party claims the bare namespace", testCert("acme", "acme-1", []string{"acme_"}, []string{"", "acme"}), "grants.feature_ns.0"},
		{"third party claims another namespace", testCert("acme", "acme-1", []string{"acme_"}, []string{"acmebank"}), "grants.feature_ns.0"},
		{"key id of another publisher", testCert("acme", "acmebank-1", []string{"acme_"}, nil), "keyid"},
		{"key id of a root", testCert("acme", "root-1", []string{"acme_"}, nil), "keyid"},
		{"publisher named like the roots", testCert("root", "root-9", []string{"root_"}, nil), "keyid"},
		{"project publisher claims another namespace", testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"quota"}, []string{"acme"}), "grants.feature_ns.0"},
	}
	for _, tc := range cases {
		err := tc.cert.Validate()
		e, ok := err.(*Error)
		if !ok || e.Code != ErrorWireInvalid || e.Params["field"] != tc.field || e.Params["rule"] != RuleRelation {
			t.Errorf("%s: %v, want relation at %s", tc.name, err, tc.field)
		}
	}
	project := testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"quota", "sso"}, []string{"", ProjectPublisher})
	if err := project.Validate(); err != nil {
		t.Fatalf("project publisher: %v", err)
	}
}

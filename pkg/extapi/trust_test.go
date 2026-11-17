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
		{"key id of a root", testCert("acme", "gwroot-1", []string{"acme_"}, nil), "keyid"},
		{"project publisher claims a third party's ids", testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"acme_"}, nil), "grants.id_prefixes.0"},
		{"project publisher claims a third party's id", testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"acme_quota"}, nil), "grants.id_prefixes.0"},
		{"project publisher claims another namespace", testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"quota"}, []string{"acme"}), "grants.feature_ns.0"},
	}
	for _, tc := range cases {
		err := tc.cert.Validate()
		e, ok := err.(*Error)
		if !ok || e.Code != ErrorWireInvalid || e.Params["field"] != tc.field || e.Params["rule"] != RuleRelation {
			t.Errorf("%s: %v, want relation at %s", tc.name, err, tc.field)
		}
	}
	// The roots' key id stem is not a publisher, so no certificate can carry
	// a root's key id as its own.
	rootStem := testCert("gwroot", "gwroot-9", []string{"gwroot_"}, nil)
	if e, ok := rootStem.Validate().(*Error); !ok || e.Params["field"] != "publisher" || e.Params["rule"] != RuleGrammar {
		t.Errorf("publisher named like the roots: %v, want grammar at publisher", rootStem.Validate())
	}
	if ValidPublisher("gwroot") || !ValidPublisher("gwroots") {
		t.Error("ValidPublisher must refuse exactly the roots' stem")
	}
	project := testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"gpuworks_", "quota", "sso"}, []string{"", ProjectPublisher})
	if err := project.Validate(); err != nil {
		t.Fatalf("project publisher: %v", err)
	}
}

// Only a project root certifies the project publisher, the bare namespace or
// same-origin console modules; an issuer the host trusts certifies the rest.
func TestRootOnlyGrantsNeedARootSignature(t *testing.T) {
	root, rootPub := newEd25519(t, RootKeyIDPrefix+"1")
	site, sitePub := newEd25519(t, "site-1")
	issuers := keySet(t, rootPub, sitePub)
	seal := func(c *PublisherCert, by ...KeySigner) *Envelope {
		t.Helper()
		raw, err := Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		env, err := Seal(raw, by...)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	project := testCert(ProjectPublisher, ProjectPublisher+"-1", []string{"quota"}, []string{"", ProjectPublisher})
	module := testCert("acme", "acme-1", []string{"acme_"}, nil)
	module.Grants.UIModule = true
	for _, tc := range []struct {
		name  string
		cert  *PublisherCert
		field string
	}{
		{"project publisher", project, "publisher"},
		{"ui module", module, "grants.ui_module"},
	} {
		_, _, err := OpenPublisherCert(seal(tc.cert, site), issuers)
		if e, ok := err.(*Error); !ok || e.Code != ErrorSignatureUntrusted || e.Params["field"] != tc.field {
			t.Errorf("%s signed by a host issuer: %v, want %s at %s", tc.name, err, ErrorSignatureUntrusted, tc.field)
		}
		if _, ids, err := OpenPublisherCert(seal(tc.cert, root, site), issuers); err != nil || len(ids) != 2 {
			t.Errorf("%s signed by a root and a host issuer: %v %v", tc.name, ids, err)
		}
	}
	plain := testCert("acme", "acme-1", []string{"acme_"}, []string{"acme"})
	plain.Grants.Roles = []string{"clock_authority"}
	if _, ids, err := OpenPublisherCert(seal(plain, site), issuers); err != nil || len(ids) != 1 || ids[0] != "site-1" {
		t.Fatalf("third-party cert signed by a host issuer: %v %v", ids, err)
	}
}

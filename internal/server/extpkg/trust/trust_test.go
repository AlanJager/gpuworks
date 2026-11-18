package trust_test

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func codeOf(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	return extapi.CodeOf(err)
}

func load(t *testing.T, st trust.Store, compiled []*extsign.Key, hosts ...trust.HostIssuer) *trust.Set {
	t.Helper()
	keys := make([]extapi.PublicKey, len(compiled))
	for i, k := range compiled {
		keys[i] = k.Public
	}
	s, err := trust.Load(st, keys, hosts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNoProjectRootIsCompiledInYet(t *testing.T) {
	if roots := trust.ProjectRoots(); len(roots) != 0 {
		t.Fatalf("a real root was compiled in before the project generated one: %v", roots)
	}
}

func TestNewRefusesAmbiguousKeys(t *testing.T) {
	r1 := extsign.NewRoot(t, "1")
	other := extsign.NewRoot(t, "1")
	if _, err := trust.New([]extapi.PublicKey{r1.Public}, nil, trust.Persisted{AddedRoots: []extapi.PublicKey{other.Public}}); err == nil {
		t.Fatal("two keys under one root key id were accepted")
	}
	host := extsign.NewKey(t, "acme-issuer")
	if _, err := trust.New(nil, []trust.HostIssuer{{Key: host.Public}, {Key: host.Public}}, trust.Persisted{}); err == nil {
		t.Fatal("a host issuer listed twice was accepted")
	}
	if _, err := trust.New(nil, []trust.HostIssuer{{Key: r1.Public}}, trust.Persisted{}); err == nil {
		t.Fatal("a host issuer under a root key id was accepted")
	}
	if _, err := trust.New([]extapi.PublicKey{host.Public}, nil, trust.Persisted{}); err == nil {
		t.Fatal("a compiled root outside the gwroot- prefix was accepted")
	}
	// Retiring the compiled root removes it.
	s, err := trust.New([]extapi.PublicKey{r1.Public}, nil, trust.Persisted{RetiredRoots: []string{r1.ID}})
	if err != nil || len(s.Roots()) != 0 {
		t.Fatalf("retired compiled root still trusted: %v %v", s.Roots(), err)
	}
}

func TestAnchorAndHostGrants(t *testing.T) {
	root := extsign.NewRoot(t, "1")
	issuer := extsign.NewKey(t, "acme-issuer")
	strict := extsign.NewKey(t, "beta-issuer")
	s, err := trust.New([]extapi.PublicKey{root.Public}, []trust.HostIssuer{
		{Key: issuer.Public, Roles: []string{"availability_authority"}, Tables: []string{"fleet_capacity"}, Authority: true, Unsandboxed: true},
		{Key: strict.Public},
	}, trust.Persisted{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Anchor([]string{issuer.ID, root.ID}); got != trust.RootAnchor {
		t.Fatalf("anchor with a root signature = %q", got)
	}
	if got := s.Anchor([]string{strict.ID, issuer.ID}); got != trust.HostAnchor(issuer.ID) {
		t.Fatalf("anchor of host issuers = %q", got)
	}
	cert := extsign.CertFor("acme", extsign.NewKey(t, "acme-k1"))
	cert.Grants.AuthorityID = "acme_lic"
	cert.Grants.IDPrefixes = []string{"acme_"}
	cert.Grants.Roles = []string{"availability_authority", "clock_authority"}
	cert.Grants.Tables = []string{"fleet_capacity", "insert_caps"}
	cert.Grants.MinIsolation = extapi.IsolationNone

	g := s.Grants(&cert, trust.RootAnchor)
	if len(g.Roles) != 2 || len(g.Tables) != 2 || g.AuthorityID != "acme_lic" || g.MinIsolation != extapi.IsolationNone {
		t.Fatalf("root grants narrowed: %+v", g)
	}
	g = s.Grants(&cert, trust.HostAnchor(issuer.ID))
	if len(g.Roles) != 1 || g.Roles[0] != "availability_authority" || len(g.Tables) != 1 || g.AuthorityID != "acme_lic" || g.MinIsolation != extapi.IsolationNone {
		t.Fatalf("allowed host grants = %+v", g)
	}
	g = s.Grants(&cert, trust.HostAnchor(strict.ID))
	if len(g.Roles) != 0 || len(g.Tables) != 0 || g.AuthorityID != "" || g.MinIsolation != extapi.IsolationLandlock || g.UIModule {
		t.Fatalf("default host grants = %+v", g)
	}
	if len(cert.Grants.Roles) != 2 {
		t.Fatal("Grants changed the certificate")
	}
}

func TestImportTrustUpdate(t *testing.T) {
	st := openStore(t)
	r1, r2 := extsign.NewRoot(t, "1"), extsign.NewRoot(t, "2")
	r3 := extsign.NewRoot(t, "3")
	s := load(t, st, []*extsign.Key{r1, r2})

	update := extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) {
		u.AddRoots = []extapi.PublicKey{r3.Public}
		u.RetireRoots = []string{r1.ID}
		u.Revoke = []extapi.Revocation{{Publisher: "acme", CertSerial: 9}}
		u.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "2.0.0"}}
	}, r1)
	in, err := trust.Import(st, s, update, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != trust.KindTrustUpdate || in.Serial != 1 || in.AddedRoots != 1 || in.RetiredRoots != 1 || in.Revocations != 1 || in.Floors != 1 {
		t.Fatalf("imported = %+v", in)
	}
	s = load(t, st, []*extsign.Key{r1, r2})
	if _, ok := s.Roots()[r1.ID]; ok {
		t.Fatal("retired root still trusted")
	}
	if _, ok := s.Roots()[r3.ID]; !ok {
		t.Fatal("added root not trusted")
	}
	if s.Floor("acme_lic") != "2.0.0" {
		t.Fatalf("floor = %q", s.Floor("acme_lic"))
	}
	cert := extsign.CertFor("acme", extsign.NewKey(t, "acme-k1"))
	cert.Serial = 9
	if s.CertRevocation(trust.RootAnchor, &cert) == nil {
		t.Fatal("revoked serial not found")
	}
	if s.CertRevocation(trust.HostAnchor("acme-issuer"), &cert) != nil {
		t.Fatal("a root revocation reached a host anchor")
	}

	// A serial already applied is a replay, even signed by a root trusted
	// now; the file signed by the retired root is no longer trusted at all.
	replay := extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) { u.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "9.0.0"}} }, r2)
	if _, err := trust.Import(st, s, replay, nil); codeOf(err) != messages.ErrorPluginTrustReplayed {
		t.Fatalf("replay = %v", err)
	}
	if _, err := trust.Import(st, s, update, nil); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("replay by the retired root = %v", err)
	}
	// A retired root no longer signs updates.
	if _, err := trust.Import(st, s, extsign.TrustUpdate(t, 2, func(u *extapi.TrustUpdate) { u.RetireRoots = []string{r2.ID} }, r1), nil); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("retired root signed = %v", err)
	}
	// The added one does.
	if _, err := trust.Import(st, s, extsign.TrustUpdate(t, 2, func(u *extapi.TrustUpdate) { u.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "2.1.0"}} }, r3), nil); err != nil {
		t.Fatal(err)
	}
	// An update re-adding a known key id with another key is refused whole.
	impostor := extsign.NewRoot(t, "2")
	conflict := extsign.TrustUpdate(t, 3, func(u *extapi.TrustUpdate) {
		u.AddRoots = []extapi.PublicKey{impostor.Public}
		u.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "9.0.0"}}
	}, r2)
	s = load(t, st, []*extsign.Key{r1, r2})
	if _, err := trust.Import(st, s, conflict, nil); codeOf(err) != messages.ErrorPluginTrustConflict {
		t.Fatalf("conflict = %v", err)
	}
	if s := load(t, st, []*extsign.Key{r1, r2}); s.Floor("acme_lic") != "2.1.0" {
		t.Fatalf("a refused update moved the floor to %s", s.Floor("acme_lic"))
	}
}

func TestImportRefusesUntrustedAndMalformedFiles(t *testing.T) {
	st := openStore(t)
	root := extsign.NewRoot(t, "1")
	stranger := extsign.NewRoot(t, "9")
	s := load(t, st, []*extsign.Key{root})
	if _, err := trust.Import(st, s, []byte("not json"), nil); codeOf(err) != extapi.ErrorWireMalformed {
		t.Fatalf("garbage = %v", err)
	}
	if _, err := trust.Import(st, s, extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) { u.RetireRoots = []string{root.ID} }, stranger), nil); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("stranger = %v", err)
	}
	// No compiled root at all (the production build today): nothing imports.
	empty := load(t, st, nil)
	if _, err := trust.Import(st, empty, extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) { u.RetireRoots = []string{root.ID} }, root), nil); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("no roots = %v", err)
	}
	// A trust update at a newer format is reported as too new, not untrusted.
	payload := []byte(`{"created":"2026-12-01T00:00:00Z","format":2,"serial":5}`)
	env, err := extapi.Seal(payload, root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.Import(st, s, extsign.Encode(t, env), nil); codeOf(err) != extapi.ErrorSchemaTooNew {
		t.Fatalf("format 2 = %v", err)
	}
}

func TestImportRevocationLists(t *testing.T) {
	st := openStore(t)
	root := extsign.NewRoot(t, "1")
	issuer := extsign.NewKey(t, "acme-issuer")
	s := load(t, st, []*extsign.Key{root}, trust.HostIssuer{Key: issuer.Public})

	rootCRL := extsign.CRL(t, 4, "", []extapi.Revocation{{Publisher: "acme", KeyID: "acme-k1"}}, root)
	in, err := trust.Import(st, s, rootCRL, nil)
	if err != nil || in.Kind != trust.KindCRL || in.Issuer != "crl:gwroot" || in.Anchor != trust.RootAnchor {
		t.Fatalf("root crl = %+v %v", in, err)
	}
	if _, err := trust.Import(st, s, rootCRL, nil); codeOf(err) != messages.ErrorPluginTrustReplayed {
		t.Fatalf("crl replay = %v", err)
	}

	// A publisher certified by a host issuer revokes one of its own builds.
	pub := extsign.NewPublisher(t, "acme", nil, issuer)
	own := extsign.CRL(t, 1, "acme", []extapi.Revocation{{Publisher: "acme", CertSerial: 1}, {Publisher: "acme", Manifest: extapi.Digest("sha256:" + hex64("b"))}}, pub.Key)
	if _, err := trust.Import(st, s, own, nil); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("publisher list without its certificate = %v", err)
	}
	in, err = trust.Import(st, s, own, pub.CertBundle(t))
	if err != nil || in.Anchor != trust.HostAnchor(issuer.ID) || in.Issuer != "crl:host:acme-issuer:acme" {
		t.Fatalf("publisher crl = %+v %v", in, err)
	}
	s = load(t, st, []*extsign.Key{root}, trust.HostIssuer{Key: issuer.Public})
	if s.CertRevocation(trust.HostAnchor(issuer.ID), &pub.Cert) == nil {
		t.Fatal("publisher's own revocation not applied under its anchor")
	}
	if s.CertRevocation(trust.RootAnchor, &pub.Cert) != nil {
		t.Fatal("a host-anchored revocation reached the root anchor")
	}
	if s.ManifestRevocation("acme", extapi.Digest("sha256:"+hex64("b"))) == nil {
		t.Fatal("manifest revocation not found")
	}
	if s.ManifestRevocation("beta", extapi.Digest("sha256:"+hex64("b"))) != nil {
		t.Fatal("a manifest revocation reached another publisher")
	}

	// The key revoked above cannot sign acme's next list: it would otherwise
	// raise the serial out of the publisher's reach.
	stolen := extsign.CRL(t, 1<<20, "acme", []extapi.Revocation{{Publisher: "acme", KeyID: "acme-k2"}}, pub.Key)
	if _, err := trust.Import(st, s, stolen, pub.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("list signed by a revoked certificate = %v", err)
	}
	if serials, _ := st.PluginTrustSerials(); serials["crl:host:acme-issuer:acme"] != 1 {
		t.Fatalf("a refused list moved the serial: %v", serials)
	}
	key2 := extsign.NewKey(t, "acme-k2")
	cert2 := extsign.CertFor("acme", key2)
	cert2.Serial = 2
	pub2 := &extsign.Publisher{Name: "acme", Key: key2, Cert: cert2, Certs: []*extapi.Envelope{extsign.Seal(t, cert2, issuer)}}
	both := &extsign.Publisher{Name: "acme", Certs: append(append([]*extapi.Envelope{}, pub.Certs...), pub2.Certs...)}
	if _, err := trust.Import(st, s, stolen, both.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("list signed by a revoked certificate beside a good one = %v", err)
	}
	next := extsign.CRL(t, 2, "acme", []extapi.Revocation{{Publisher: "acme", Manifest: extapi.Digest("sha256:" + hex64("c"))}}, key2)
	if in, err := trust.Import(st, s, next, both.CertBundle(t)); err != nil || in.Serial != 2 {
		t.Fatalf("list signed by the publisher's new certificate = %+v %v", in, err)
	}

	// Another publisher's key cannot sign acme's list.
	beta := extsign.NewPublisher(t, "beta", nil, root)
	forged := extsign.CRL(t, 2, "acme", []extapi.Revocation{{Publisher: "acme"}}, beta.Key)
	if _, err := trust.Import(st, s, forged, beta.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("forged list = %v", err)
	}
}

func hex64(c string) string {
	out := ""
	for range 64 {
		out += c
	}
	return out
}

// failingStore fails each method on demand, to prove errors propagate and
// nothing is reported applied.
type failingStore struct {
	trust.Store
	roots, revs, floors, apply error
}

func (f failingStore) PluginRoots() ([]extapi.PublicKey, []string, error) {
	if f.roots != nil {
		return nil, nil, f.roots
	}
	return f.Store.PluginRoots()
}
func (f failingStore) PluginRevocations() (map[string][]extapi.Revocation, error) {
	if f.revs != nil {
		return nil, f.revs
	}
	return f.Store.PluginRevocations()
}
func (f failingStore) PluginFloors() (map[string]string, error) {
	if f.floors != nil {
		return nil, f.floors
	}
	return f.Store.PluginFloors()
}
func (f failingStore) ApplyPluginTrust(issuer, anchor string, u extapi.TrustUpdate) (bool, uint64, error) {
	if f.apply != nil {
		return false, 0, f.apply
	}
	return f.Store.ApplyPluginTrust(issuer, anchor, u)
}

func TestStoreFailuresPropagate(t *testing.T) {
	st := openStore(t)
	boom := errors.New("database locked")
	for name, f := range map[string]failingStore{
		"roots": {Store: st, roots: boom}, "revocations": {Store: st, revs: boom}, "floors": {Store: st, floors: boom},
	} {
		if _, err := trust.Load(f, nil, nil); !errors.Is(err, boom) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	root := extsign.NewRoot(t, "1")
	s := load(t, st, []*extsign.Key{root})
	if _, err := trust.Import(failingStore{Store: st, apply: boom}, s, extsign.TrustUpdate(t, 1, func(u *extapi.TrustUpdate) { u.RetireRoots = []string{root.ID} }, root), nil); !errors.Is(err, boom) {
		t.Fatalf("apply failure = %v", err)
	}
}

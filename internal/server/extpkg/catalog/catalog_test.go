package catalog_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/extpkg/catalog"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func codeOf(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	return extapi.CodeOf(err)
}

type fixture struct {
	st   *store.Store
	root *extsign.Key
	pub  *extsign.Publisher
	dir  layout.Dir
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := extsign.NewRoot(t, "1")
	return &fixture{st: st, root: root, pub: extsign.NewPublisher(t, "acme", nil, root), dir: layout.New(t.TempDir())}
}

func (f *fixture) set(t *testing.T) *trust.Set {
	t.Helper()
	s, err := trust.Load(f.st, []extapi.PublicKey{f.root.Public}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) entry(t *testing.T, id, version string, requires map[string]int) extapi.IndexEntry {
	t.Helper()
	spec := extsign.Minimal("acme", id, version)
	spec.Manifest.Requires = requires
	spec.Manifest.MinCoreHint = "v27.3"
	b := extsign.Build(t, f.pub, spec)
	return b.IndexEntry(t, id+"_"+version+".gwpkg", b.Archive(t))
}

func TestImportAppliesAndSavesTheIndex(t *testing.T) {
	f := newFixture(t)
	b100 := f.entry(t, "acme_backup", "1.0.0", nil)
	b110 := f.entry(t, "acme_backup", "1.1.0", map[string]int{"table.availability": 2})
	lic := f.entry(t, "acme_lic", "2.0.3", nil)
	revokedBuild := f.entry(t, "acme_lic", "2.0.1", nil)
	idx := extsign.Index(t, f.pub, 7, []extapi.IndexEntry{b100, b110, revokedBuild, lic}, func(x *extapi.Index) {
		x.Revoked = []extapi.Revocation{{Publisher: "acme", Manifest: revokedBuild.Manifest, Replacement: "2.0.3"}}
		x.Floors = []extapi.Floor{{ID: "acme_lic", MinVersion: "2.0.0"}}
	})
	in, err := catalog.Import(f.st, f.set(t), f.dir, idx, f.pub.CertBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if in.Index.Serial != 7 || in.Anchor != trust.RootAnchor || in.Issuer != "index:gwroot:acme" {
		t.Fatalf("imported = %+v", in)
	}
	set := f.set(t)
	if set.Floor("acme_lic") != "2.0.0" || set.ManifestRevocation("acme", revokedBuild.Manifest) == nil {
		t.Fatal("the index's floors and revocations were not applied")
	}
	indexes, errs, err := catalog.Load(set, f.dir)
	if err != nil || len(errs) != 0 || len(indexes) != 1 {
		t.Fatalf("load = %v %v %v", indexes, errs, err)
	}
	choices := catalog.Choose(indexes[0], map[string]int{"table.availability": 1}, set)
	if len(choices) != 2 {
		t.Fatalf("choices = %+v", choices)
	}
	backup := choices[0]
	if backup.ID != "acme_backup" || backup.Best == nil || backup.Best.Version != "1.0.0" || len(backup.Newer) != 1 {
		t.Fatalf("acme_backup = %+v", backup)
	}
	if codeOf(backup.Newer[0].Err) != messages.ErrorPluginNeedsNewerCore {
		t.Fatalf("1.1.0 refused with %v", backup.Newer[0].Err)
	}
	var pe *messages.ProductError
	if errors.As(backup.Newer[0].Err, &pe); pe.Params()["min_core_hint"] != "v27.3" || pe.Params()["capability"] != "table.availability" {
		t.Fatalf("needs-newer params = %v", pe.Params())
	}
	licChoice := choices[1]
	if licChoice.Best == nil || licChoice.Best.Version != "2.0.3" || len(licChoice.Newer) != 0 {
		t.Fatalf("acme_lic = %+v", licChoice)
	}
	// With a newer server the newest build is chosen.
	if c := catalog.Choose(indexes[0], map[string]int{"table.availability": 2}, set); c[0].Best.Version != "1.1.0" {
		t.Fatalf("newer server chose %s", c[0].Best.Version)
	}

	// Replays are refused and keep the saved index.
	if _, err := catalog.Import(f.st, set, f.dir, idx, f.pub.CertBundle(t)); codeOf(err) != messages.ErrorPluginTrustReplayed {
		t.Fatalf("replay = %v", err)
	}
	older := extsign.Index(t, f.pub, 6, []extapi.IndexEntry{b100}, nil)
	if _, err := catalog.Import(f.st, set, f.dir, older, f.pub.CertBundle(t)); codeOf(err) != messages.ErrorPluginTrustReplayed {
		t.Fatalf("older index = %v", err)
	}
	saved, _ := os.ReadFile(filepath.Join(f.dir.Root, ".index", "acme", layout.IndexFile))
	if string(saved) != string(idx) {
		t.Fatal("a refused index replaced the saved one")
	}
}

func TestImportRefusesForgedIndexes(t *testing.T) {
	f := newFixture(t)
	b := f.entry(t, "acme_backup", "1.0.0", nil)
	beta := extsign.NewPublisher(t, "beta", nil, f.root)
	// beta signs an index claiming to be acme's.
	forged := extsign.Encode(t, extsign.Seal(t, extapi.Index{Format: 1, Serial: 1, Created: extapi.NewTime(extsign.Epoch), Publisher: "acme", Packages: []extapi.IndexEntry{b}}, beta.Key))
	if _, err := catalog.Import(f.st, f.set(t), f.dir, forged, beta.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("forged = %v", err)
	}
	if _, err := catalog.Import(f.st, f.set(t), f.dir, forged, f.pub.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("forged with acme's certificate = %v", err)
	}
	stranger := extsign.NewPublisher(t, "acme", nil, extsign.NewRoot(t, "9"))
	if _, err := catalog.Import(f.st, f.set(t), f.dir, extsign.Index(t, stranger, 1, []extapi.IndexEntry{b}, nil), stranger.CertBundle(t)); codeOf(err) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("untrusted publisher = %v", err)
	}
	if _, err := catalog.Import(f.st, f.set(t), f.dir, []byte("{}"), nil); codeOf(err) != extapi.ErrorWireInvalid {
		t.Fatalf("garbage = %v", err)
	}
	if indexes, _ := f.dir.Indexes(); len(indexes) != 0 {
		t.Fatalf("a refused index was saved: %v", indexes)
	}
}

func TestLoadReportsIndexesThatNoLongerVerify(t *testing.T) {
	f := newFixture(t)
	idx := extsign.Index(t, f.pub, 1, []extapi.IndexEntry{f.entry(t, "acme_backup", "1.0.0", nil)}, nil)
	if _, err := catalog.Import(f.st, f.set(t), f.dir, idx, f.pub.CertBundle(t)); err != nil {
		t.Fatal(err)
	}
	// The root that certified acme is retired.
	empty, err := trust.New(nil, nil, trust.Persisted{})
	if err != nil {
		t.Fatal(err)
	}
	indexes, errs, err := catalog.Load(empty, f.dir)
	if err != nil || len(indexes) != 0 || codeOf(errs["acme"]) != extapi.ErrorSignatureUntrusted {
		t.Fatalf("load = %v %v %v", indexes, errs, err)
	}
}

func TestChooseSkipsRevokedAndBelowFloorBuilds(t *testing.T) {
	f := newFixture(t)
	b1 := f.entry(t, "acme_backup", "1.0.0", nil)
	b2 := f.entry(t, "acme_backup", "1.1.0", nil)
	b3 := f.entry(t, "acme_backup", "1.2.0", nil)
	b3.Protocol = extapi.Range{Min: 2, Max: 2}
	x := &extapi.Index{Publisher: "acme", Packages: []extapi.IndexEntry{b1, b2, b3}}
	set, err := trust.New([]extapi.PublicKey{f.root.Public}, nil, trust.Persisted{
		Revocations: map[string][]extapi.Revocation{trust.RootAnchor: {{Publisher: "acme", Manifest: b2.Manifest}}},
		Floors:      map[string]string{"acme_backup": "1.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := catalog.Choose(x, nil, set)[0]
	if c.Best != nil || len(c.Newer) != 3 {
		t.Fatalf("choice = %+v", c)
	}
	for i, want := range []string{extapi.ErrorProtocolUnsupported, messages.ErrorPluginPackageRevoked, messages.ErrorPluginBelowFloor} {
		if codeOf(c.Newer[i].Err) != want {
			t.Fatalf("refusal %d = %v, want %s", i, c.Newer[i].Err, want)
		}
	}
}

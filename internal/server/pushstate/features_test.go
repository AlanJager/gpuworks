package pushstate

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Features lists exactly the keys pushed state answers for, sorted, with
// the presentation the manifest declared: a key outside its publisher's
// namespace, an invalid key and a second declaration are left out.
func TestFeaturesAreTheDeclaredKeysPushedStateAnswersFor(t *testing.T) {
	stray := Package{ID: "acme_stray", Publisher: "acme", Grants: acmeGrants, Verified: true, Features: []extapi.FeatureDecl{
		{Key: "acme.backup", Presentation: extapi.PresentationConceal, Gate: extapi.GateSelf},  // acme_backup declared it first
		{Key: "rogue.thing", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf},  // another publisher's namespace
		{Key: "acme.Bad Key", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}, // not a key
	}}
	s := New(Config{Store: openStore(t, ""), Packages: append(append([]Package(nil), allPackages...), stray)})
	want := []featuregate.Feature{
		{Key: "acme.backup", Presentation: featuregate.Promote},
		{Key: "acme.free", Presentation: featuregate.Promote},
		{Key: "acme.reports", Presentation: featuregate.Promote},
		{Key: "acme.tools", Presentation: featuregate.Conceal},
		{Key: "audit", Presentation: featuregate.Promote},
	}
	if got := s.Features(); !reflect.DeepEqual(got, want) {
		t.Fatalf("features = %+v", got)
	}
	if _, err := featuregate.New(s.Features(), s); err != nil {
		t.Fatalf("the gate refuses the features: %v", err)
	}
	s.SetPackages(nil)
	if got := s.Features(); len(got) != 0 {
		t.Fatalf("with nothing installed: %+v", got)
	}
}
